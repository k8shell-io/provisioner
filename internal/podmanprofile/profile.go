// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package podmanprofile expands the blueprint's podman.profile into the
// concrete podman settings the workspace Helm chart renders. A profile only
// supplies defaults: every value the blueprint sets explicitly wins, except the
// profile's own securityContext keys (see applySecurityContext).
package podmanprofile

import (
	"errors"
	"fmt"

	"github.com/k8shell-io/common/pkg/models"
	"gopkg.in/yaml.v3"
)

// Profile names accepted in podman.profile.
const (
	Privileged = "privileged"
	Rootless   = "rootless"
	Kata       = "kata"
)

const (
	// GraphStorage is the podman storage a profile adjusts.
	GraphStorage = "graphdb"

	containersConfPath = "/etc/containers/containers.conf"
	storageConfPath    = "/etc/containers/storage.conf"

	// DefaultCacheTrimSeconds is how often the kata profile drops the guest page cache.
	DefaultCacheTrimSeconds = 300
	// MinCacheTrimSeconds is the shortest interval a blueprint may set.
	MinCacheTrimSeconds = 30
	// DefaultStorageMountPath is where podman keeps its graph root.
	DefaultStorageMountPath = "/var/lib/containers/storage"
	// DefaultFileDiskPath is where the volume holding storage.img is mounted.
	DefaultFileDiskPath = "/var/lib/containers/disk"
	// DefaultBlockDevicePath is where a raw block volume appears.
	DefaultBlockDevicePath = "/dev/podman-disk"
)

const privilegedContainersConf = `[containers]
log_driver = "k8s-file"
[engine]
cgroup_manager = "cgroupfs"
events_logger="file"
runtime="crun"
`

const rootlessContainersConf = `[containers]
netns="host"
userns="host"
ipcns="host"
utsns="host"
cgroupns="host"
cgroups="disabled"
log_driver = "k8s-file"
volumes = ["/proc:/proc"]
default_sysctls = []
[engine]
cgroup_manager = "cgroupfs"
events_logger="file"
runtime="crun"
`

const rootlessStorageConf = `[storage]
driver = "overlay"
rootless_storage_path = "/home/podman/storage"
`

// kataStorageConf uses the kernel overlay directly on the ext4 volume: no
// fuse-overlayfs mount_program and none of its mount options (the podman
// image default has fsync=0, which the kernel overlay rejects with EINVAL).
const kataStorageConf = `[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "/var/lib/containers/storage"
[storage.options.overlay]
mountopt = "nodev"
`

// kataStartupScript prepares the guest before podman starts. Kata gives
// containers only a minimal /dev, and mounts a cgroupfs that does not match
// the container's cgroup namespace, so crun could not start any container.
const kataStartupScript = `[ -e /dev/fuse ] || mknod -m 666 /dev/fuse c 10 229
[ -e /dev/kmsg ] || mknod -m 600 /dev/kmsg c 1 11
[ -e /dev/loop-control ] || mknod -m 660 /dev/loop-control c 10 237
for i in 0 1 2 3 4 5 6 7; do [ -e /dev/loop$i ] || mknod -m 660 /dev/loop$i b 7 $i; done
umount /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup`

// ErrKataRuntimeClass is returned when the kata profile is used without the
// blueprint naming its runtime class.
var ErrKataRuntimeClass = errors.New("podman profile kata requires runtimeClassName to be set in the blueprint")

// Valid reports whether name is a known profile.
func Valid(name string) bool {
	switch name {
	case Privileged, Rootless, Kata:
		return true
	}
	return false
}

// Effective returns the blueprint with its podman profile applied. When no
// profile applies (none set, or podman disabled) bp itself is returned;
// otherwise a deep copy is expanded and bp is left untouched.
func Effective(bp *models.Blueprint, user *models.User) (*models.Blueprint, error) {
	if bp == nil || bp.Podman.Profile == "" || !bp.Podman.Enabled {
		return bp, nil
	}

	raw, err := yaml.Marshal(bp)
	if err != nil {
		return nil, fmt.Errorf("marshal blueprint: %w", err)
	}
	var out models.Blueprint
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unmarshal blueprint copy: %w", err)
	}
	out.Metadata = bp.Metadata

	if err := Apply(&out, user); err != nil {
		return nil, err
	}
	return &out, nil
}

// Apply expands bp.Podman.Profile in place, filling only values the blueprint
// left unset. It is a no-op when no profile is set or podman is disabled.
func Apply(bp *models.Blueprint, user *models.User) error {
	p := &bp.Podman
	if p.Profile == "" || !p.Enabled {
		return nil
	}

	switch p.Profile {
	case Privileged:
		applySecurityContext(p, map[string]interface{}{
			"privileged": true,
			"runAsUser":  0,
			"runAsGroup": 0,
		})
		setFile(p, containersConfPath, privilegedContainersConf)
		applyGraphStorage(p, DefaultStorageMountPath, intPtr(0), intPtr(0))

	case Rootless:
		if user == nil {
			return fmt.Errorf("podman profile %q needs the workspace user", Rootless)
		}
		applySecurityContext(p, map[string]interface{}{
			"appArmorProfile": map[string]interface{}{"type": "Unconfined"},
			"runAsUser":       int(user.UID),
			"runAsGroup":      int(user.GID),
		})
		setFile(p, containersConfPath, rootlessContainersConf)
		setFile(p, storageConfPath, rootlessStorageConf)
		setFile(p, "/etc/group", fmt.Sprintf("root:x:0:\n%s:x:%d:\n", user.Username, user.GID))
		setFile(p, "/etc/passwd", fmt.Sprintf("root:x:0:0:root:/root:/bin/sh\n%s:x:%d:%d::/home/podman:/bin/sh\n", user.Username, user.UID, user.GID))
		setFile(p, "/etc/subgid", fmt.Sprintf("%s:2000000000:65536\n", user.Username))
		setFile(p, "/etc/subuid", fmt.Sprintf("%s:2000000000:65536\n", user.Username))
		// The owner defaults to the workspace user in the chart.
		applyGraphStorage(p, "/home/podman", nil, nil)

	case Kata:
		if bp.RuntimeClassName == "" {
			return ErrKataRuntimeClass
		}
		applySecurityContext(p, map[string]interface{}{
			"privileged":      true,
			"runAsUser":       0,
			"runAsGroup":      0,
			"appArmorProfile": map[string]interface{}{"type": "Unconfined"},
		})
		setFile(p, containersConfPath, privilegedContainersConf)
		setFile(p, storageConfPath, kataStorageConf)
		if p.StartupScript == "" {
			p.StartupScript = kataStartupScript
		}
		if p.SocketMedium == "" {
			p.SocketMedium = "Memory"
		}
		if p.CacheTrimSeconds == nil {
			p.CacheTrimSeconds = intPtr(DefaultCacheTrimSeconds)
		}
		applyStorageImage(bp)
		diskPath := p.StorageImage.DiskPath
		applyGraphStorage(p, diskPath, intPtr(0), intPtr(0))

	default:
		return fmt.Errorf("unknown podman profile %q", p.Profile)
	}
	return nil
}

// applyStorageImage fills the storage image defaults of the kata profile. The
// default mode is file; size defaults to 90% of the claim so a sparse image can
// never outgrow the volume.
func applyStorageImage(bp *models.Blueprint) {
	p := &bp.Podman
	if p.StorageImage == nil {
		p.StorageImage = &models.PodmanStorageImage{}
	}
	si := p.StorageImage
	if si.Mode == "" {
		si.Mode = "file"
	}
	if si.Storage == "" {
		si.Storage = GraphStorage
	}
	if si.MountPath == "" {
		si.MountPath = DefaultStorageMountPath
	}
	if si.DiskPath == "" {
		if si.Mode == "block" {
			si.DiskPath = DefaultBlockDevicePath
		} else {
			si.DiskPath = DefaultFileDiskPath
		}
	}
	if si.InitImage == "" {
		// Must provide mkfs.ext4 (e2fsprogs) and blkid.
		si.InitImage = bp.Image
	}
	if si.Mode == "file" && si.Size == "" {
		if claim, ok := StorageSize(p.Storages[si.Storage]); ok {
			si.Size = fmt.Sprintf("%d", claim*9/10)
		}
	}
}

// applySecurityContext overlays the profile's security context on the
// blueprint's. Unlike every other profile default, the profile's keys win: a
// blueprint inherits podman.securityContext from its base template, so an
// inherited value can't be told from an authored one, and a profile can't work
// with another profile's user or without its privileged flag. Keys the profile
// doesn't set (capabilities, seccompProfile, ...) are kept.
func applySecurityContext(p *models.Podman, sc map[string]interface{}) {
	merged := make(map[string]interface{}, len(p.SecurityContext)+len(sc))
	for k, v := range p.SecurityContext {
		merged[k] = v
	}
	for k, v := range sc {
		merged[k] = v
	}
	p.SecurityContext = merged
}

// setFile adds an ext file unless the blueprint already provides that path.
func setFile(p *models.Podman, path, content string) {
	if p.ExtFiles == nil {
		p.ExtFiles = map[string]string{}
	}
	if _, ok := p.ExtFiles[path]; !ok {
		p.ExtFiles[path] = content
	}
}

// applyGraphStorage fills path and owner of the blueprint's graph storage. The
// storage itself (claim spec, size, class) is infrastructure the blueprint
// owns, so a profile never creates it.
func applyGraphStorage(p *models.Podman, path string, uid, gid *int) {
	s, ok := p.Storages[GraphStorage]
	if !ok || !s.Enabled {
		return
	}
	if s.Path == "" {
		s.Path = path
	}
	if s.FsOwnerUid == nil {
		s.FsOwnerUid = uid
	}
	if s.FsOwnerGid == nil {
		s.FsOwnerGid = gid
	}
	p.Storages[GraphStorage] = s
}

func intPtr(i int) *int { return &i }
