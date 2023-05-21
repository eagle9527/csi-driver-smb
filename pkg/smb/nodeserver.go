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

package smb

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"golang.org/x/net/context"
)

// NodePublishVolume mount the volume from staging to target path
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	source := req.GetStagingTargetPath()
	if len(source) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target not provided")
	}

	mountOptions := []string{"bind"}
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	mnt, err := d.ensureMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount target %q: %v", target, err)
	}
	if mnt {
		klog.V(2).Infof("NodePublishVolume: %s is already mounted", target)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	if err = preparePublishPath(target, d.mounter); err != nil {
		return nil, fmt.Errorf("prepare publish failed for %s with error: %v", target, err)
	}

	klog.V(2).Infof("NodePublishVolume: mounting %s at %s with mountOptions: %v volumeID(%s)", source, target, mountOptions, volumeID)
	if err := d.mounter.Mount(source, target, "", mountOptions); err != nil {
		if removeErr := os.Remove(target); removeErr != nil {
			return nil, status.Errorf(codes.Internal, "Could not remove mount target %q: %v", target, removeErr)
		}
		return nil, status.Errorf(codes.Internal, "Could not mount %q at %q: %v", source, target, err)
	}
	klog.V(2).Infof("NodePublishVolume: mount %s at %s volumeID(%s) successfully", source, target, volumeID)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmount the volume from the target path
func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	klog.V(2).Infof("NodeUnpublishVolume: unmounting volume %s on %s", volumeID, targetPath)
	err := CleanupMountPoint(d.mounter, targetPath, true /*extensiveMountPointCheck*/)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount target %q: %v", targetPath, err)
	}
	klog.V(2).Infof("NodeUnpublishVolume: unmount volume %s on %s successfully", volumeID, targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeStageVolume mount the volume to a staging path
func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	volumeCapability := req.GetVolumeCapability()
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	targetPath := req.GetStagingTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target not provided")
	}

	context := req.GetVolumeContext()
	mountFlags := req.GetVolumeCapability().GetMount().GetMountFlags()
	volumeMountGroup := req.GetVolumeCapability().GetMount().GetVolumeMountGroup()
	secrets := req.GetSecrets()
	gidPresent := checkGidPresentInMountFlags(mountFlags)

	var source, subDir string
	subDirReplaceMap := map[string]string{}
	for k, v := range context {
		switch strings.ToLower(k) {
		case sourceField:
			source = v
		case subDirField:
			subDir = v
		case pvcNamespaceKey:
			subDirReplaceMap[pvcNamespaceMetadata] = v
		case pvcNameKey:
			subDirReplaceMap[pvcNameMetadata] = v
		case pvNameKey:
			subDirReplaceMap[pvNameMetadata] = v
		}
	}

	if source == "" {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("%s field is missing, current context: %v", sourceField, context))
	}

	if acquired := d.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, volumeOperationAlreadyExistsFmt, volumeID)
	}
	defer d.volumeLocks.Release(volumeID)

	var username, password, domain string
	for k, v := range secrets {
		switch strings.ToLower(k) {
		case usernameField:
			username = strings.TrimSpace(v)
		case passwordField:
			password = strings.TrimSpace(v)
		case domainField:
			domain = strings.TrimSpace(v)
		}
	}

	// in guest login, username and password options are not needed
	requireUsernamePwdOption := !hasGuestMountOptions(mountFlags)

	var mountOptions, sensitiveMountOptions []string
	if runtime.GOOS == "windows" {
		if domain == "" {
			domain = defaultDomainName
		}
		if requireUsernamePwdOption {
			if !strings.Contains(username, "\\") {
				username = fmt.Sprintf("%s\\%s", domain, username)
			}
			mountOptions = []string{username}
			sensitiveMountOptions = []string{password}
		}
	} else {
		var useKerberosCache, err = ensureKerberosCache(volumeID, mountFlags, secrets)
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Error writing kerberos cache: %v", err))
		}
		if err := os.MkdirAll(targetPath, 0750); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("MkdirAll %s failed with error: %v", targetPath, err))
		}
		if requireUsernamePwdOption && !useKerberosCache {
			sensitiveMountOptions = []string{fmt.Sprintf("%s=%s,%s=%s", usernameField, username, passwordField, password)}
		}
		mountOptions = mountFlags
		if !gidPresent && volumeMountGroup != "" {
			mountOptions = append(mountOptions, fmt.Sprintf("gid=%s", volumeMountGroup))
		}
		if domain != "" {
			mountOptions = append(mountOptions, fmt.Sprintf("%s=%s", domainField, domain))
		}
	}

	klog.V(2).Infof("NodeStageVolume: targetPath(%v) volumeID(%v) context(%v) mountflags(%v) mountOptions(%v)",
		targetPath, volumeID, context, mountFlags, mountOptions)

	isDirMounted, err := d.ensureMountPoint(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount target %s: %v", targetPath, err)
	}
	if isDirMounted {
		klog.V(2).Infof("NodeStageVolume: already mounted volume %s on target %s", volumeID, targetPath)
	} else {
		if err = prepareStagePath(targetPath, d.mounter); err != nil {
			return nil, fmt.Errorf("prepare stage path failed for %s with error: %v", targetPath, err)
		}
		if subDir != "" {
			// replace pv/pvc name namespace metadata in subDir
			subDir = replaceWithMap(subDir, subDirReplaceMap)

			source = strings.TrimRight(source, "/")
			source = fmt.Sprintf("%s/%s", source, subDir)
		}
		mountComplete := false
		err = wait.PollImmediate(1*time.Second, 2*time.Minute, func() (bool, error) {
			err := Mount(d.mounter, source, targetPath, "cifs", mountOptions, sensitiveMountOptions)
			mountComplete = true
			return true, err
		})
		if !mountComplete {
			return nil, status.Error(codes.Internal, fmt.Sprintf("volume(%s) mount %q on %q failed with timeout(10m)", volumeID, source, targetPath))
		}
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("volume(%s) mount %q on %q failed with %v", volumeID, source, targetPath, err))
		}
		klog.V(2).Infof("volume(%s) mount %q on %q succeeded", volumeID, source, targetPath)
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unmount the volume from the staging path
func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target not provided")
	}

	if acquired := d.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, volumeOperationAlreadyExistsFmt, volumeID)
	}
	defer d.volumeLocks.Release(volumeID)

	klog.V(2).Infof("NodeUnstageVolume: CleanupMountPoint on %s with volume %s", stagingTargetPath, volumeID)
	if err := CleanupSMBMountPoint(d.mounter, stagingTargetPath, true /*extensiveMountPointCheck*/); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount staging target %q: %v", stagingTargetPath, err)
	}

	if err := deleteKerberosCache(volumeID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete kerberos cache: %v", err)
	}

	klog.V(2).Infof("NodeUnstageVolume: unmount volume %s on %s successfully", volumeID, stagingTargetPath)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodeGetCapabilities return the capabilities of the Node plugin
func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: d.NSCap,
	}, nil
}

// NodeGetInfo return info of the node on which this plugin is running
func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: d.NodeID,
	}, nil
}

// NodeGetVolumeStats get volume stats
func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if len(req.VolumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume ID was empty")
	}
	if len(req.VolumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume path was empty")
	}

	if _, err := os.Lstat(req.VolumePath); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "path %s does not exist", req.VolumePath)
		}
		return nil, status.Errorf(codes.Internal, "failed to stat file %s: %v", req.VolumePath, err)
	}

	volumeMetrics, err := volume.NewMetricsStatFS(req.VolumePath).GetMetrics()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get metrics: %v", err)
	}

	available, ok := volumeMetrics.Available.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume available size(%v)", volumeMetrics.Available)
	}
	capacity, ok := volumeMetrics.Capacity.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume capacity size(%v)", volumeMetrics.Capacity)
	}
	used, ok := volumeMetrics.Used.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume used size(%v)", volumeMetrics.Used)
	}

	inodesFree, ok := volumeMetrics.InodesFree.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes free(%v)", volumeMetrics.InodesFree)
	}
	inodes, ok := volumeMetrics.Inodes.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes(%v)", volumeMetrics.Inodes)
	}
	inodesUsed, ok := volumeMetrics.InodesUsed.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes used(%v)", volumeMetrics.InodesUsed)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Available: available,
				Total:     capacity,
				Used:      used,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
			},
		},
	}, nil
}

// NodeExpandVolume node expand volume
// N/A for smb
func (d *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// ensureMountPoint: create mount point if not exists
// return <true, nil> if it's already a mounted point otherwise return <false, nil>
func (d *Driver) ensureMountPoint(target string) (bool, error) {
	notMnt, err := d.mounter.IsLikelyNotMountPoint(target)
	if err != nil && !os.IsNotExist(err) {
		if IsCorruptedDir(target) {
			notMnt = false
			klog.Warningf("detected corrupted mount for targetPath [%s]", target)
		} else {
			return !notMnt, err
		}
	}

	if runtime.GOOS != "windows" {
		// Check all the mountpoints in case IsLikelyNotMountPoint
		// cannot handle --bind mount
		mountList, err := d.mounter.List()
		if err != nil {
			return !notMnt, err
		}

		targetAbs, err := filepath.Abs(target)
		if err != nil {
			return !notMnt, err
		}

		for _, mountPoint := range mountList {
			if mountPoint.Path == targetAbs {
				notMnt = false
				break
			}
		}
	}

	if !notMnt {
		// testing original mount point, make sure the mount link is valid
		_, err := os.ReadDir(target)
		if err == nil {
			klog.V(2).Infof("already mounted to target %s", target)
			return !notMnt, nil
		}
		// mount link is invalid, now unmount and remount later
		klog.Warningf("ReadDir %s failed with %v, unmount this directory", target, err)
		if err := d.mounter.Unmount(target); err != nil {
			klog.Errorf("Unmount directory %s failed with %v", target, err)
			return !notMnt, err
		}
		notMnt = true
		return !notMnt, err
	}

	if err := makeDir(target); err != nil {
		klog.Errorf("MakeDir failed on target: %s (%v)", target, err)
		return !notMnt, err
	}

	return false, nil
}

func makeDir(pathname string) error {
	err := os.MkdirAll(pathname, os.FileMode(0755))
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func checkGidPresentInMountFlags(mountFlags []string) bool {
	for _, mountFlag := range mountFlags {
		if strings.HasPrefix(mountFlag, "gid") {
			return true
		}
	}
	return false
}

func hasKerberosMountOption(mountFlags []string) bool {
	for _, mountFlag := range mountFlags {
		if strings.HasPrefix(mountFlag, "sec=krb5") {
			return true
		}
	}
	return false
}

func getCredUID(mountFlags []string) (int, error) {
	var cruidPrefix = "cruid="
	for _, mountFlag := range mountFlags {
		if strings.HasPrefix(mountFlag, cruidPrefix) {
			return strconv.Atoi(strings.TrimPrefix(mountFlag, cruidPrefix))
		}
	}
	return -1, fmt.Errorf("Can't find credUid in mount flags")
}

func getKrb5CcacheName(credUID int) string {
	return fmt.Sprintf("%s%d", krb5Prefix, credUID)
}

// returns absolute path for name of file inside krb5CacheDirectory
func getKerberosFilePath(fileName string) string {
	return fmt.Sprintf("%s%s", krb5CacheDirectory, fileName)
}

func volumeKerberosCacheName(volumeID string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(volumeID))
	return strings.ReplaceAll(strings.ReplaceAll(encoded, "/", "-"), "+", "_")
}

func kerberosCacheDirectoryExists() (bool, error) {
	_, err := os.Stat(krb5CacheDirectory)
	if os.IsNotExist(err) {
		return false, status.Error(codes.Internal, fmt.Sprintf("Directory for kerberos caches must exist, it will not be created: %s: %v", krb5CacheDirectory, err))
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func getKerberosCache(credUID int, secrets map[string]string) (string, []byte, error) {
	var krb5CcacheName = getKrb5CcacheName(credUID)
	var krb5CcacheContent string
	for k, v := range secrets {
		switch strings.ToLower(k) {
		case krb5CcacheName:
			krb5CcacheContent = v
		}
	}
	if krb5CcacheContent == "" {
		return "", nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Empty kerberos cache in key %s", krb5CcacheName))
	}
	content, err := base64.StdEncoding.DecodeString(krb5CcacheContent)
	if err != nil {
		return "", nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Malformed kerberos cache in key %s, expected to be in base64 form: %v", krb5CcacheName, err))
	}
	var krb5CacheFileName = getKerberosFilePath(getKrb5CcacheName(credUID))

	return krb5CacheFileName, content, nil
}

// Create kerberos cache in the file based on the VolumeID, so it can be cleaned up during unstage
// At the same time, kerberos expects to find cache in file named "krb5cc_*", so creating symlink
// will allow both clean up and serving proper cache to the kerberos.
func ensureKerberosCache(volumeID string, mountFlags []string, secrets map[string]string) (bool, error) {
	var securityIsKerberos = hasKerberosMountOption(mountFlags)
	if securityIsKerberos {
		_, err := kerberosCacheDirectoryExists()
		if err != nil {
			return false, err
		}
		credUID, err := getCredUID(mountFlags)
		if err != nil {
			return false, err
		}
		krb5CacheFileName, content, err := getKerberosCache(credUID, secrets)
		if err != nil {
			return false, err
		}
		// Write cache into volumeId-based filename, so it can be cleaned up later
		volumeIDCacheFileName := volumeKerberosCacheName(volumeID)

		volumeIDCacheAbsolutePath := getKerberosFilePath(volumeIDCacheFileName)
		if err := os.WriteFile(volumeIDCacheAbsolutePath, content, os.FileMode(0700)); err != nil {
			return false, status.Error(codes.Internal, fmt.Sprintf("Couldn't write kerberos cache to file %s: %v", volumeIDCacheAbsolutePath, err))
		}
		if err := os.Chown(volumeIDCacheAbsolutePath, credUID, credUID); err != nil {
			return false, status.Error(codes.Internal, fmt.Sprintf("Couldn't chown kerberos cache %s to user %d: %v", volumeIDCacheAbsolutePath, credUID, err))
		}

		if _, err := os.Stat(krb5CacheFileName); os.IsNotExist(err) {
			klog.Warningf("symlink file doesn't exist, it'll be created [%s]", krb5CacheFileName)
		} else {
			if err := os.Remove(krb5CacheFileName); err != nil {
				klog.Warningf("couldn't delete the file [%s]", krb5CacheFileName)
			}
		}

		// Create symlink to the cache file with expected name
		if err := os.Symlink(volumeIDCacheAbsolutePath, krb5CacheFileName); err != nil {
			return false, status.Error(codes.Internal, fmt.Sprintf("Couldn't create symlink to a cache file %s->%s to user %d: %v", krb5CacheFileName, volumeIDCacheFileName, credUID, err))
		}

		return true, nil
	}
	return false, nil
}

func deleteKerberosCache(volumeID string) error {
	exists, err := kerberosCacheDirectoryExists()
	// If not supported, simply return
	if !exists {
		return nil
	}
	if err != nil {
		return err
	}

	volumeIDCacheFileName := volumeKerberosCacheName(volumeID)

	var volumeIDCacheAbsolutePath = getKerberosFilePath(volumeIDCacheFileName)
	_, err = os.Stat(volumeIDCacheAbsolutePath)
	// Not created or already removed
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}

	// If file with cache exists, full clean means removing symlinks to the file.
	dirEntries, _ := os.ReadDir(krb5CacheDirectory)
	for _, dirEntry := range dirEntries {
		filePath := getKerberosFilePath(dirEntry.Name())
		lStat, _ := os.Lstat(filePath)
		// If it's a symlink, checking if it's pointing to the volume file in question
		if lStat != nil {
			target, _ := os.Readlink(filePath)
			if target == volumeIDCacheAbsolutePath {
				err = os.Remove(filePath)
				if err != nil {
					klog.Errorf("Error removing symlink to kerberos ticket cache: %s (%v)", filePath, err)
				}
			}
		}
	}

	err = os.Remove(volumeIDCacheAbsolutePath)
	if err != nil {
		klog.Errorf("Error removing symlink to kerberos ticket cache: %s (%v)", volumeIDCacheAbsolutePath, err)
	}

	return nil
}
