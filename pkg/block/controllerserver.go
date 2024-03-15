/*
Copyright 2020 The Kubernetes Authors.

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

package block

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"k8s.io/klog/v2"
)

// ControllerServer controller server setting
type ControllerServer struct {
	Driver *Driver
}

// blockVolume is an internal representation of a volume
// created by the provisioner.
type blockVolume struct {
	// Volume id
	id string
	// file of the BLOCK
	blockFile string
	// fstype of volume
	fstype string
	// size of volume
	size int64
	// pv name when subDir is not empty
	uuid string
	// on delete action
	onDelete string
}

// blockSnapshot is an internal representation of a volume snapshot
// created by the provisioner.
type blockSnapshot struct {
	// Snapshot id.
	id string
	// Base directory of the BLOCK server to create snapshots under
	// Matches paramShare.
	blockFile string
	// fstype of volume
	fstype string
	// Snapshot name.
	uuid string
	// Source volume.
	src string
}

func (snap blockSnapshot) archiveName() string {
	return fmt.Sprintf("%v.tar.gz", snap.src)
}

// Ordering of elements in the CSI volume id.
// ID is of the form {BlockFIle}.
// TODO: This volume id format limits baseDir and
// subDir to only be one directory deep.
// Adding a new element should always go at the end
// before totalIDElements
const (
	idBlockFile = iota
	idFstype
	idUUID
	idOnDelete
	totalIDElements // Always last
)

// Ordering of elements in the CSI snapshot id.
// ID is of the form {blocfile}.
// Adding a new element should always go at the end
// before totalSnapIDElements
const (
	idSnapBlockFile = iota
	idSnapFstype
	idSnapUUID
	idSnapArchivePath
	idSnapArchiveName
	totalIDSnapElements // Always last
)

// CreateVolume create a volume
func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if len(name) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume name must be provided")
	}

	if err := isValidVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	mountPermissions := cs.Driver.mountPermissions
	reqCapacity := req.GetCapacityRange().GetRequiredBytes()
	parameters := req.GetParameters()
	if parameters == nil {
		parameters = make(map[string]string)
	}
	// validate parameters (case-insensitive)
	for k, v := range parameters {
		switch strings.ToLower(k) {
		case paramBlockFile:
		case paramFsType:
		case paramOnDelete:
		case pvcNamespaceKey:
		case pvcNameKey:
		case pvNameKey:
			// no op
		case mountPermissionsField:
			if v != "" {
				var err error
				if mountPermissions, err = strconv.ParseUint(v, 8, 32); err != nil {
					return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid mountPermissions %s in storage class", v))
				}
			}
		default:
			return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid parameter %q in storage class", k))
		}
	}

	blockVol, err := newBLOCKVolume(name, reqCapacity, parameters, cs.Driver.defaultOnDeletePolicy)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	var volCap *csi.VolumeCapability
	if len(req.GetVolumeCapabilities()) > 0 {
		volCap = req.GetVolumeCapabilities()[0]
	}
	// Mount block base share so we can create a subdirectory
	if err = cs.internalMount(ctx, blockVol, parameters, volCap); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to mount block file: %v", err.Error())
	}
	defer func() {
		if err = cs.internalUnmount(ctx, blockVol); err != nil {
			klog.Warningf("failed to unmount block file: %v", err.Error())
		}
	}()

	// Create subdirectory under base-dir
	internalVolumePath := getInternalVolumePath(cs.Driver.workingMountDir, blockVol)

	if mountPermissions > 0 {
		// Reset directory permissions because of umask problems
		if err = os.Chmod(internalVolumePath, os.FileMode(mountPermissions)); err != nil {
			klog.Warningf("failed to chmod subdirectory: %v", err.Error())
		}
	}

	if req.GetVolumeContentSource() != nil {
		if err := cs.copyVolume(ctx, req, blockVol); err != nil {
			return nil, err
		}
	}

	setKeyValueInMap(parameters, paramBlockFile, blockVol.blockFile)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      blockVol.id,
			CapacityBytes: 0, // by setting it to zero, Provisioner will use PVC requested size as PV size
			VolumeContext: parameters,
			ContentSource: req.GetVolumeContentSource(),
		},
	}, nil
}

// DeleteVolume delete a volume
func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is empty")
	}
	blockVol, err := getBlockVolFromID(volumeID)
	if err != nil {
		// An invalid ID should be treated as doesn't exist
		klog.Warningf("failed to get block volume for volume id %v deletion: %v", volumeID, err)
		return &csi.DeleteVolumeResponse{}, nil
	}

	if blockVol.onDelete == "" {
		blockVol.onDelete = cs.Driver.defaultOnDeletePolicy
	}

	if !strings.EqualFold(blockVol.onDelete, retain) {
		// mount block base share so we can delete the subdirectory
		if err = cs.internalUnmount(ctx, blockVol); err != nil {
			klog.Warningf("failed to unmount block server: %v", err.Error())
		}

		if strings.EqualFold(blockVol.onDelete, archive) {
			klog.V(2).Infof("ArchiveVolume: volume(%s) is not support", volumeID)
		} else {
			// delete subdirectory under base-dir
			klog.V(2).Infof("removing block at %v", blockVol.blockFile)
			if err = os.RemoveAll(blockVol.blockFile); err != nil {
				return nil, status.Errorf(codes.Internal, "delete block(%s) failed with %v", blockVol, err.Error())
			}
		}
	} else {
		klog.V(2).Infof("DeleteVolume: volume(%s) is set to retain, not deleting/archiving subdirectory", volumeID)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *ControllerServer) ControllerPublishVolume(_ context.Context, _ *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ControllerUnpublishVolume(_ context.Context, _ *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ControllerGetVolume(_ context.Context, _ *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if err := isValidVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
		Message: "",
	}, nil
}

func (cs *ControllerServer) ListVolumes(_ context.Context, _ *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) GetCapacity(_ context.Context, _ *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetCapabilities implements the default GRPC callout.
// Default supports all capabilities
func (cs *ControllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.Driver.cscap,
	}, nil
}

func (cs *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateSnapshot name must be provided")
	}
	if len(req.GetSourceVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateSnapshot source volume ID must be provided")
	}

	srcVol, err := getBlockVolFromID(req.GetSourceVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "failed to create source volume: %v", err)
	}
	snapshot, err := newBLOCKSnapshot(req.GetName(), req.GetParameters(), srcVol)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "failed to create blockSnapshot: %v", err)
	}
	snapVol := volumeFromSnapshot(snapshot)
	if err = cs.internalMount(ctx, snapVol, nil, nil); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to mount snapshot block server: %v", err)
	}
	defer func() {
		if err = cs.internalUnmount(ctx, snapVol); err != nil {
			klog.Warningf("failed to unmount snapshot block server: %v", err)
		}
	}()
	snapInternalVolPath := getInternalVolumePath(cs.Driver.workingMountDir, snapVol)
	if err = os.MkdirAll(snapInternalVolPath, 0o777); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to make subdirectory: %v", err)
	}
	if err := validateSnapshot(snapInternalVolPath, snapshot); err != nil {
		return nil, err
	}

	if err = cs.internalMount(ctx, srcVol, nil, nil); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to mount src block server: %v", err)
	}
	defer func() {
		if err = cs.internalUnmount(ctx, srcVol); err != nil {
			klog.Warningf("failed to unmount src block server: %v", err)
		}
	}()

	srcPath := getInternalVolumePath(cs.Driver.workingMountDir, srcVol)
	dstPath := filepath.Join(snapInternalVolPath, snapshot.archiveName())
	klog.V(2).Infof("archiving %v -> %v", srcPath, dstPath)
	out, err := exec.Command("tar", "-C", srcPath, "-czvf", dstPath, ".").CombinedOutput()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create archive for snapshot: %v: %v", err, string(out))
	}
	klog.V(2).Infof("archived %s -> %s", srcPath, dstPath)

	var snapshotSize int64
	fi, err := os.Stat(dstPath)
	if err != nil {
		klog.Warningf("failed to determine snapshot size: %v", err)
	} else {
		snapshotSize = fi.Size()
	}
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SnapshotId:     snapshot.id,
			SourceVolumeId: srcVol.id,
			SizeBytes:      snapshotSize,
			CreationTime:   timestamppb.Now(),
			ReadyToUse:     true,
		},
	}, nil
}

func (cs *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if len(req.GetSnapshotId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Snapshot ID is required for deletion")
	}
	snap, err := getBlockSnapFromID(req.GetSnapshotId())
	if err != nil {
		// An invalid ID should be treated as doesn't exist
		klog.Warningf("failed to get block snapshot for id %v deletion: %v", req.GetSnapshotId(), err)
		return &csi.DeleteSnapshotResponse{}, nil
	}

	var volCap *csi.VolumeCapability
	mountOptions := getMountOptions(req.GetSecrets())
	if mountOptions != "" {
		klog.V(2).Infof("DeleteSnapshot: found mountOptions(%s) for snapshot(%s)", mountOptions, req.GetSnapshotId())
		volCap = &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{
					MountFlags: []string{mountOptions},
				},
			},
		}
	}
	vol := volumeFromSnapshot(snap)
	if err = cs.internalMount(ctx, vol, nil, volCap); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to mount block server for snapshot deletion: %v", err)
	}
	defer func() {
		if err = cs.internalUnmount(ctx, vol); err != nil {
			klog.Warningf("failed to unmount block server after snapshot deletion: %v", err)
		}
	}()

	// delete snapshot archive
	internalVolumePath := getInternalVolumePath(cs.Driver.workingMountDir, vol)
	klog.V(2).Infof("Removing snapshot archive at %v", internalVolumePath)
	if err = os.RemoveAll(internalVolumePath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete subdirectory: %v", err.Error())
	}

	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *ControllerServer) ListSnapshots(_ context.Context, _ *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ControllerExpandVolume(_ context.Context, _ *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// Mount block server at base-dir
func (cs *ControllerServer) internalMount(ctx context.Context, vol *blockVolume, volumeContext map[string]string, volCap *csi.VolumeCapability) error {
	if volCap == nil {
		volCap = &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		}
	}

	blcokFilePath := filepath.Join(string(filepath.Separator) + vol.blockFile)
	targetPath := getInternalMountPath(cs.Driver.workingMountDir, vol)

	volContext := map[string]string{
		paramFsType:    vol.fstype,
		paramBlockFile: blcokFilePath,
	}
	for k, v := range volumeContext {
		// don't set subDir field since only block-server:/share should be mounted in CreateVolume/DeleteVolume
		if strings.ToLower(k) != paramBlockFile {
			volContext[k] = v
		}
	}

	klog.V(2).Infof("internally mounting %s at %s", vol.blockFile, targetPath)
	_, err := cs.Driver.ns.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		TargetPath:       targetPath,
		VolumeContext:    volContext,
		VolumeCapability: volCap,
		VolumeId:         vol.id,
	})
	return err
}

// Unmount block server at base-dir
func (cs *ControllerServer) internalUnmount(ctx context.Context, vol *blockVolume) error {
	targetPath := getInternalMountPath(cs.Driver.workingMountDir, vol)

	// Unmount block server at base-dir
	klog.V(4).Infof("internally unmounting %v", targetPath)
	_, err := cs.Driver.ns.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   vol.id,
		TargetPath: targetPath,
	})
	return err
}

func (cs *ControllerServer) copyFromSnapshot(ctx context.Context, req *csi.CreateVolumeRequest, dstVol *blockVolume) error {
	snap, err := getBlockSnapFromID(req.VolumeContentSource.GetSnapshot().GetSnapshotId())
	if err != nil {
		return status.Error(codes.NotFound, err.Error())
	}
	snapVol := volumeFromSnapshot(snap)

	var volCap *csi.VolumeCapability
	if len(req.GetVolumeCapabilities()) > 0 {
		volCap = req.GetVolumeCapabilities()[0]
	}

	if err = cs.internalMount(ctx, snapVol, nil, volCap); err != nil {
		return status.Errorf(codes.Internal, "failed to mount src block server for snapshot volume copy: %v", err)
	}
	defer func() {
		if err = cs.internalUnmount(ctx, snapVol); err != nil {
			klog.Warningf("failed to unmount src block server after snapshot volume copy: %v", err)
		}
	}()
	if err = cs.internalMount(ctx, dstVol, nil, volCap); err != nil {
		return status.Errorf(codes.Internal, "failed to mount dst block server for snapshot volume copy: %v", err)
	}
	defer func() {
		if err = cs.internalUnmount(ctx, dstVol); err != nil {
			klog.Warningf("failed to unmount dst block server after snapshot volume copy: %v", err)
		}
	}()

	// untar snapshot archive to dst path
	snapPath := filepath.Join(getInternalVolumePath(cs.Driver.workingMountDir, snapVol), snap.archiveName())
	dstPath := getInternalVolumePath(cs.Driver.workingMountDir, dstVol)
	klog.V(2).Infof("copy volume from snapshot %v -> %v", snapPath, dstPath)
	out, err := exec.Command("tar", "-xzvf", snapPath, "-C", dstPath).CombinedOutput()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to copy volume for snapshot: %v: %v", err, string(out))
	}
	klog.V(2).Infof("volume copied from snapshot %v -> %v", snapPath, dstPath)
	return nil
}

func (cs *ControllerServer) copyFromVolume(ctx context.Context, req *csi.CreateVolumeRequest, dstVol *blockVolume) error {
	srcVol, err := getBlockVolFromID(req.GetVolumeContentSource().GetVolume().GetVolumeId())
	if err != nil {
		return status.Error(codes.NotFound, err.Error())
	}
	// Note that the source path must include trailing '/.', can't use 'filepath.Join()' as it performs path cleaning
	srcPath := fmt.Sprintf("%v/.", getInternalVolumePath(cs.Driver.workingMountDir, srcVol))
	dstPath := getInternalVolumePath(cs.Driver.workingMountDir, dstVol)
	klog.V(2).Infof("copy volume from volume %v -> %v", srcPath, dstPath)

	var volCap *csi.VolumeCapability
	if len(req.GetVolumeCapabilities()) > 0 {
		volCap = req.GetVolumeCapabilities()[0]
	}
	if err = cs.internalMount(ctx, srcVol, nil, volCap); err != nil {
		return status.Errorf(codes.Internal, "failed to mount src block server: %v", err)
	}
	defer func() {
		if err = cs.internalUnmount(ctx, srcVol); err != nil {
			klog.Warningf("failed to unmount block server: %v", err)
		}
	}()
	if err = cs.internalMount(ctx, dstVol, nil, volCap); err != nil {
		return status.Errorf(codes.Internal, "failed to mount dst block server: %v", err)
	}
	defer func() {
		if err = cs.internalUnmount(ctx, dstVol); err != nil {
			klog.Warningf("failed to unmount dst block server: %v", err)
		}
	}()

	// recursive 'cp' with '-a' to handle symlinks
	out, err := exec.Command("cp", "-a", srcPath, dstPath).CombinedOutput()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to copy volume %v: %v", err, string(out))
	}
	klog.V(2).Infof("copied %s -> %s", srcPath, dstPath)
	return nil
}

func (cs *ControllerServer) copyVolume(ctx context.Context, req *csi.CreateVolumeRequest, vol *blockVolume) error {
	vs := req.VolumeContentSource
	switch vs.Type.(type) {
	case *csi.VolumeContentSource_Snapshot:
		return cs.copyFromSnapshot(ctx, req, vol)
	case *csi.VolumeContentSource_Volume:
		return cs.copyFromVolume(ctx, req, vol)
	default:
		return status.Errorf(codes.InvalidArgument, "%v not a proper volume source", vs)
	}
}

// newBLOCKSnapshot Convert VolumeSnapshot parameters to a blockSnapshot
func newBLOCKSnapshot(name string, params map[string]string, vol *blockVolume) (*blockSnapshot, error) {
	blockFile := vol.blockFile
	for k, v := range params {
		switch strings.ToLower(k) {
		case paramFsType:
			blockFile = v
		default:
			return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid parameter %q in snapshot storage class", k))
		}
	}

	if blockFile == "" {
		return nil, fmt.Errorf("%v is a required parameter", paramFsType)
	}
	snapshot := &blockSnapshot{
		blockFile: blockFile,
		uuid:      name,
	}
	if vol.blockFile != "" {
		snapshot.src = vol.blockFile
	}
	if vol.uuid != "" {
		snapshot.src = vol.uuid
	}
	if snapshot.src == "" {
		return nil, fmt.Errorf("missing required source volume name")
	}
	snapshot.id = getSnapshotIDFromBlockSnapshot(snapshot)
	return snapshot, nil
}

// newBLOCKVolume Convert VolumeCreate parameters to an blockVolume
func newBLOCKVolume(name string, size int64, params map[string]string, defaultOnDeletePolicy string) (*blockVolume, error) {
	var blockFile, fstype, onDelete string
	subDirReplaceMap := map[string]string{}

	// validate parameters (case-insensitive)
	for k, v := range params {
		switch strings.ToLower(k) {
		case paramFsType:
			fstype = v
		case paramBlockFile:
			blockFile = v
		case paramOnDelete:
			onDelete = v
		case pvcNamespaceKey:
			subDirReplaceMap[pvcNamespaceMetadata] = v
		case pvcNameKey:
			subDirReplaceMap[pvcNameMetadata] = v
		case pvNameKey:
			subDirReplaceMap[pvNameMetadata] = v
		}
	}

	if blockFile == "" {
		return nil, fmt.Errorf("%v is a required parameter", paramFsType)
	}

	vol := &blockVolume{
		blockFile: blockFile,
		fstype:    fstype,
		size:      size,
	}
	vol.blockFile = name

	if err := validateOnDeleteValue(onDelete); err != nil {
		return nil, err
	}

	vol.onDelete = defaultOnDeletePolicy
	if onDelete != "" {
		vol.onDelete = onDelete
	}

	vol.id = getVolumeIDFromBlockVol(vol)
	return vol, nil
}

// getInternalMountPath: get working directory for CreateVolume and DeleteVolume
func getInternalMountPath(workingMountDir string, vol *blockVolume) string {
	if vol == nil {
		return ""
	}
	mountDir := vol.uuid
	if vol.uuid == "" {
		mountDir = vol.blockFile
	}
	return filepath.Join(workingMountDir, mountDir)
}

// Get internal path where the volume is created
// The reason why the internal path is "workingDir/subDir/subDir" is because:
//   - the semantic is actually "workingDir/volId/subDir" and volId == subDir.
//   - we need a mount directory per volId because you can have multiple
//     CreateVolume calls in parallel and they may use the same underlying share.
//     Instead of refcounting how many CreateVolume calls are using the same
//     share, it's simpler to just do a mount per request.
func getInternalVolumePath(workingMountDir string, vol *blockVolume) string {
	return getInternalMountPath(workingMountDir, vol)
}

// Given a blockVolume, return a CSI volume id
func getVolumeIDFromBlockVol(vol *blockVolume) string {
	idElements := make([]string, totalIDElements)
	idElements[idBlockFile] = strings.Trim(vol.blockFile, "/")
	idElements[idFstype] = vol.fstype
	idElements[idUUID] = vol.uuid
	if strings.EqualFold(vol.onDelete, retain) || strings.EqualFold(vol.onDelete, archive) {
		idElements[idOnDelete] = vol.onDelete
	}

	return strings.Join(idElements, separator)
}

// Given a blockSnapshot, return a CSI snapshot id.
func getSnapshotIDFromBlockSnapshot(snap *blockSnapshot) string {
	idElements := make([]string, totalIDSnapElements)
	idElements[idBlockFile] = strings.Trim(snap.blockFile, "/")
	idElements[idFstype] = snap.fstype
	idElements[idSnapUUID] = snap.uuid
	idElements[idSnapArchivePath] = snap.uuid
	idElements[idSnapArchiveName] = snap.src
	return strings.Join(idElements, separator)
}

// Given a CSI volume id, return a blockVolume
// sample volume Id:
//
//	  new volumeID:
//		    block-server.default.svc.cluster.local#share#pvc-4bcbf944-b6f7-4bd0-b50f-3c3dd00efc64
//		    block-server.default.svc.cluster.local#share#subdir#pvc-4bcbf944-b6f7-4bd0-b50f-3c3dd00efc64#retain
//	  old volumeID: block-server.default.svc.cluster.local/share/pvc-4bcbf944-b6f7-4bd0-b50f-3c3dd00efc64
func getBlockVolFromID(id string) (*blockVolume, error) {
	var blockFile, fstype, uuid, onDelete string
	segments := strings.Split(id, separator)
	blockFile = segments[idBlockFile]
	fstype = segments[idFstype]
	if len(segments) >= 3 {
		uuid = segments[idUUID]
	}
	if len(segments) >= 4 {
		onDelete = segments[idOnDelete]
	}

	return &blockVolume{
		id:        id,
		blockFile: blockFile,
		fstype:    fstype,
		uuid:      uuid,
		onDelete:  onDelete,
	}, nil
}

// Given a CSI snapshot ID, return a blockSnapshot
// sample snapshot ID:
//
//	block-server.default.svc.cluster.local#share#snapshot-016f784f-56f4-44d1-9041-5f59e82dbce1#snapshot-016f784f-56f4-44d1-9041-5f59e82dbce1#pvc-4bcbf944-b6f7-4bd0-b50f-3c3dd00efc64
func getBlockSnapFromID(id string) (*blockSnapshot, error) {
	segments := strings.Split(id, separator)
	if len(segments) == totalIDSnapElements {
		return &blockSnapshot{
			id:        id,
			blockFile: segments[idSnapBlockFile],
			fstype:    segments[idSnapFstype],
			src:       segments[idSnapArchiveName],
			uuid:      segments[idSnapUUID],
		}, nil
	}

	return &blockSnapshot{}, fmt.Errorf("failed to create blockSnapshot from snapshot ID")
}

// isValidVolumeCapabilities validates the given VolumeCapability array is valid
func isValidVolumeCapabilities(volCaps []*csi.VolumeCapability) error {
	if len(volCaps) == 0 {
		return fmt.Errorf("volume capabilities missing in request")
	}
	for _, c := range volCaps {
		if c.GetBlock() != nil {
			return fmt.Errorf("block volume capability not supported")
		}
	}
	return nil
}

// Validate snapshot after internal mount
func validateSnapshot(snapInternalVolPath string, snap *blockSnapshot) error {
	return filepath.WalkDir(snapInternalVolPath, func(path string, d fs.DirEntry, err error) error {
		if path == snapInternalVolPath {
			// skip root
			return nil
		}
		if err != nil {
			return err
		}
		if d.Name() != snap.archiveName() {
			// there should be just one archive in the snapshot path and archive name should match
			return status.Errorf(codes.AlreadyExists, "snapshot with the same name but different source volume ID already exists: found %q, desired %q", d.Name(), snap.archiveName())
		}
		return nil
	})
}

// Volume for snapshot internal mount/unmount
func volumeFromSnapshot(snap *blockSnapshot) *blockVolume {
	return &blockVolume{
		id:        snap.id,
		blockFile: snap.blockFile,
		fstype:    snap.fstype,
		uuid:      snap.uuid,
	}
}
