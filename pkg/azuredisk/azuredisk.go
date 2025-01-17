/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azuredisk

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-12-01/compute"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pborman/uuid"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/mount-utils"

	csicommon "sigs.k8s.io/azuredisk-csi-driver/pkg/csi-common"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/mounter"
	volumehelper "sigs.k8s.io/azuredisk-csi-driver/pkg/util"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider"
	azure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

const (
	// DriverName driver name
	DriverName       = "disk.csi.azure.com"
	azurePublicCloud = "AZUREPUBLICCLOUD"
	azureStackCloud  = "AZURESTACKCLOUD"

	errDiskNotFound = "not found"
	// default IOPS Caps & Throughput Cap (MBps) per https://docs.microsoft.com/en-us/azure/virtual-machines/linux/disks-ultra-ssd
	// see https://docs.microsoft.com/en-us/rest/api/compute/disks/createorupdate#uri-parameters
	diskNameMinLength = 1
	diskNameMaxLength = 80
	// maxLength = 80 - (4 for ".vhd") = 76
	diskNameGenerateMaxLength = 76

	// minimum disk size is 1 GiB
	minimumDiskSizeGiB = 1

	resourceNotFound = "ResourceNotFound"

	// VolumeAttributes for Partition
	volumeAttributePartition = "partition"

	// see https://docs.microsoft.com/en-us/rest/api/compute/disks/createorupdate#create-a-managed-disk-by-copying-a-snapshot.
	diskSnapshotPath = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/snapshots/%s"

	// see https://docs.microsoft.com/en-us/rest/api/compute/disks/createorupdate#create-a-managed-disk-from-an-existing-managed-disk-in-the-same-or-different-subscription.
	managedDiskPath = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s"

	// LUN lun number
	LUN = "LUN"

	cachingModeField        = "cachingmode"
	storageAccountTypeField = "storageaccounttype"
	skuNameField            = "skuname"
	locationField           = "location"
	resourceGroupField      = "resourcegroup"
	diskIOPSReadWriteField  = "diskiopsreadwrite"
	diskMBPSReadWriteField  = "diskmbpsreadwrite"
	diskNameField           = "diskname"
	desIDField              = "diskencryptionsetid"
	tagsField               = "tags"
	maxSharesField          = "maxshares"
	incrementalField        = "incremental"
	logicalSectorSizeField  = "logicalsectorsize"
	fsTypeField             = "fstype"
	kindField               = "kind"

	WellKnownTopologyKey = "topology.kubernetes.io/zone"
)

var (
	managedDiskPathRE       = regexp.MustCompile(`(?i).*/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/disks/(.+)`)
	diskSnapshotPathRE      = regexp.MustCompile(`(?i).*/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/snapshots/(.+)`)
	diskURISupportedManaged = []string{"/subscriptions/{sub-id}/resourcegroups/{group-name}/providers/microsoft.compute/disks/{disk-id}"}
)

// CSIDriver defines the interface for a CSI driver.
type CSIDriver interface {
	csi.ControllerServer
	csi.NodeServer
	csi.IdentityServer

	Run(endpoint, kubeconfig string, testMode bool)
}

// DriverCore contains fields common to both the V1 and V2 driver, and implements all interfaces of CSI drivers
type DriverCore struct {
	csicommon.CSIDriver
	cloud   *azure.Cloud
	mounter *mount.SafeFormatAndMount
}

// Driver is the v1 implementation of the Azure Disk CSI Driver.
type Driver struct {
	DriverCore
	volumeLocks *volumehelper.VolumeLocks
}

// newDriverV1 Creates a NewCSIDriver object. Assumes vendor version is equal to driver version &
// does not support optional driver plugin info manifest field. Refer to CSI spec for more details.
func newDriverV1(nodeID string) *Driver {
	driver := Driver{}
	driver.Name = DriverName
	driver.Version = driverVersion
	driver.NodeID = nodeID
	driver.volumeLocks = volumehelper.NewVolumeLocks()
	return &driver
}

// Run driver initialization
func (d *Driver) Run(endpoint, kubeconfig string, testBool bool) {
	versionMeta, err := GetVersionYAML()
	if err != nil {
		klog.Fatalf("%v", err)
	}
	klog.Infof("\nDRIVER INFORMATION:\n-------------------\n%s\n\nStreaming logs below:", versionMeta)
	cloud, err := GetCloudProvider(kubeconfig)
	if err != nil || cloud.TenantID == "" || cloud.SubscriptionID == "" {
		klog.Fatalf("failed to get Azure Cloud Provider, error: %v", err)
	}
	d.cloud = cloud

	if d.NodeID == "" {
		// Disable UseInstanceMetadata for controller to mitigate a timeout issue using IMDS
		// https://github.com/kubernetes-sigs/azuredisk-csi-driver/issues/168
		klog.Infoln("disable UseInstanceMetadata for controller")
		d.cloud.Config.UseInstanceMetadata = false
	}

	d.mounter, err = mounter.NewSafeMounter()
	if err != nil {
		klog.Fatalf("Failed to get safe mounter. Error: %v", err)
	}

	d.AddControllerServiceCapabilities(
		[]csi.ControllerServiceCapability_RPC_Type{
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
			csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
			csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
			csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
			csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
			csi.ControllerServiceCapability_RPC_LIST_VOLUMES_PUBLISHED_NODES,
		})
	d.AddVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER})
	d.AddNodeServiceCapabilities([]csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	})

	s := csicommon.NewNonBlockingGRPCServer()
	// Driver d act as IdentityServer, ControllerServer and NodeServer
	s.Start(endpoint, d, d, d, testBool)
	s.Wait()
}

func GetDiskName(diskURI string) (string, error) {
	matches := managedDiskPathRE.FindStringSubmatch(diskURI)
	if len(matches) != 2 {
		return "", fmt.Errorf("could not get disk name from %s, correct format: %s", diskURI, managedDiskPathRE)
	}
	return matches[1], nil
}

func getSnapshotName(snapshotURI string) (string, error) {
	matches := diskSnapshotPathRE.FindStringSubmatch(snapshotURI)
	if len(matches) != 2 {
		return "", fmt.Errorf("could not get snapshot name from %s, correct format: %s", snapshotURI, diskSnapshotPathRE)
	}
	return matches[1], nil
}

func GetResourceGroupFromURI(diskURI string) (string, error) {
	fields := strings.Split(diskURI, "/")
	if len(fields) != 9 || strings.ToLower(fields[3]) != "resourcegroups" {
		return "", fmt.Errorf("invalid disk URI: %s", diskURI)
	}
	return fields[4], nil
}

func (d *Driver) checkDiskExists(ctx context.Context, diskURI string) error {
	diskName, err := GetDiskName(diskURI)
	if err != nil {
		return err
	}

	resourceGroup, err := GetResourceGroupFromURI(diskURI)
	if err != nil {
		return err
	}

	if _, rerr := d.cloud.DisksClient.Get(ctx, resourceGroup, diskName); rerr != nil {
		return rerr.Error()
	}

	return nil
}

func (d *Driver) checkDiskCapacity(ctx context.Context, resourceGroup, diskName string, requestGiB int) (bool, error) {
	disk, err := d.cloud.DisksClient.Get(ctx, resourceGroup, diskName)
	// Because we can not judge the reason of the error. Maybe the disk does not exist.
	// So here we do not handle the error.
	if err == nil {
		if !reflect.DeepEqual(disk, compute.Disk{}) && disk.DiskSizeGB != nil && int(*disk.DiskSizeGB) != requestGiB {
			return false, status.Errorf(codes.AlreadyExists, "the request volume already exists, but its capacity(%v) is different from (%v)", *disk.DiskProperties.DiskSizeGB, requestGiB)
		}
	}
	return true, nil
}

func (d *Driver) getVolumeLocks() *volumehelper.VolumeLocks {
	return d.volumeLocks
}

func isValidDiskURI(diskURI string) error {
	if strings.Index(strings.ToLower(diskURI), "/subscriptions/") != 0 {
		return fmt.Errorf("Inavlid DiskURI: %v, correct format: %v", diskURI, diskURISupportedManaged)
	}
	return nil
}

// isARMResourceID check whether resourceID is an ARM ResourceID
func isARMResourceID(resourceID string) bool {
	id := strings.ToLower(resourceID)
	return strings.Contains(id, "/subscriptions/")
}

// Disk name must begin with a letter or number, end with a letter, number or underscore,
// and may contain only letters, numbers, underscores, periods, or hyphens.
// See https://docs.microsoft.com/en-us/rest/api/compute/disks/createorupdate#uri-parameters
//
//
// Snapshot name must begin with a letter or number, end with a letter, number or underscore,
// and may contain only letters, numbers, underscores, periods, or hyphens.
// See https://docs.microsoft.com/en-us/rest/api/compute/snapshots/createorupdate#uri-parameters
//
// Since the naming rule of disk is same with snapshot's, here we use the same function to handle disks and snapshots.
func getValidDiskName(volumeName string) string {
	diskName := volumeName
	if len(diskName) > diskNameMaxLength {
		diskName = diskName[0:diskNameMaxLength]
		klog.Warningf("since the maximum volume name length is %d, so it is truncated as (%q)", diskNameMaxLength, diskName)
	}
	if !checkDiskName(diskName) || len(diskName) < diskNameMinLength {
		// todo: get cluster name
		diskName = util.GenerateVolumeName("pvc-disk", uuid.NewUUID().String(), diskNameGenerateMaxLength)
		klog.Warningf("the requested volume name (%q) is invalid, so it is regenerated as (%q)", volumeName, diskName)
	}

	return diskName
}

func checkDiskName(diskName string) bool {
	length := len(diskName)

	for i, v := range diskName {
		if !(unicode.IsLetter(v) || unicode.IsDigit(v) || v == '_' || v == '.' || v == '-') ||
			(i == 0 && !(unicode.IsLetter(v) || unicode.IsDigit(v))) ||
			(i == length-1 && !(unicode.IsLetter(v) || unicode.IsDigit(v) || v == '_')) {
			return false
		}
	}

	return true
}

func getSourceVolumeID(snapshot *compute.Snapshot) string {
	if snapshot != nil &&
		snapshot.SnapshotProperties != nil &&
		snapshot.SnapshotProperties.CreationData != nil &&
		snapshot.SnapshotProperties.CreationData.SourceResourceID != nil {
		return *snapshot.SnapshotProperties.CreationData.SourceResourceID
	}
	return ""
}

func getValidCreationData(subscriptionID, resourceGroup, sourceResourceID, sourceType string) (compute.CreationData, error) {
	if sourceResourceID == "" {
		return compute.CreationData{
			CreateOption: compute.Empty,
		}, nil
	}

	switch sourceType {
	case sourceSnapshot:
		if match := diskSnapshotPathRE.FindString(sourceResourceID); match == "" {
			sourceResourceID = fmt.Sprintf(diskSnapshotPath, subscriptionID, resourceGroup, sourceResourceID)
		}

	case sourceVolume:
		if match := managedDiskPathRE.FindString(sourceResourceID); match == "" {
			sourceResourceID = fmt.Sprintf(managedDiskPath, subscriptionID, resourceGroup, sourceResourceID)
		}
	default:
		return compute.CreationData{
			CreateOption: compute.Empty,
		}, nil
	}

	splits := strings.Split(sourceResourceID, "/")
	if len(splits) > 9 {
		if sourceType == sourceSnapshot {
			return compute.CreationData{}, fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", sourceResourceID, diskSnapshotPathRE)
		}

		return compute.CreationData{}, fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", sourceResourceID, managedDiskPathRE)
	}
	return compute.CreationData{
		CreateOption:     compute.Copy,
		SourceResourceID: &sourceResourceID,
	}, nil
}

// isAvailabilityZone returns true if the zone is in format of <region>-<zone-id>.
func isAvailabilityZone(zone, region string) bool {
	return strings.HasPrefix(zone, fmt.Sprintf("%s-", region))
}

func IsCorruptedDir(dir string) bool {
	_, pathErr := mount.PathExists(dir)
	fmt.Printf("IsCorruptedDir(%s) returned with error: %v", dir, pathErr)
	return pathErr != nil && mount.IsCorruptedMnt(pathErr)
}

// setControllerCapabilities sets the controller capabilities field. It is intended for use with unit tests.
func (d *DriverCore) setControllerCapabilities(caps []*csi.ControllerServiceCapability) {
	d.Cap = caps
}

// setNodeCapabilities sets the node capabilities field. It is intended for use with unit tests.
func (d *DriverCore) setNodeCapabilities(nodeCaps []*csi.NodeServiceCapability) {
	d.NSCap = nodeCaps
}

// setName sets the Name field. It is intended for use with unit tests.
func (d *DriverCore) setName(name string) {
	d.Name = name
}

// setName sets the NodeId field. It is intended for use with unit tests.
func (d *DriverCore) setNodeID(nodeID string) {
	d.NodeID = nodeID
}

// setName sets the Version field. It is intended for use with unit tests.
func (d *DriverCore) setVersion(version string) {
	d.Version = version
}

// getCloud returns the value of the cloud field. It is intended for use with unit tests.
func (d *DriverCore) getCloud() *provider.Cloud {
	return d.cloud
}

// setCloud sets the cloud field. It is intended for use with unit tests.
func (d *DriverCore) setCloud(cloud *provider.Cloud) {
	d.cloud = cloud
}

// getMounter returns the value of the mounter field. It is intended for use with unit tests.
func (d *DriverCore) getMounter() *mount.SafeFormatAndMount {
	return d.mounter
}

// setMounter sets the mounter field. It is intended for use with unit tests.
func (d *DriverCore) setMounter(mounter *mount.SafeFormatAndMount) {
	d.mounter = mounter
}
