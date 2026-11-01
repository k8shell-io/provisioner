// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package podmanprofile

import (
	"errors"
	"strings"
	"testing"

	"github.com/k8shell-io/common/pkg/models"
)

func testBlueprint(profile string) *models.Blueprint {
	rc := ""
	if profile == Kata {
		rc = "kata-qemu"
	}
	return &models.Blueprint{
		RuntimeClassName: rc,
		Image:            "registry.example/dev:1",
		Podman: models.Podman{
			Enabled: true,
			Profile: profile,
			Storages: map[string]models.Storage{
				"graphdb": {
					Enabled: true,
					ClaimSpec: map[string]interface{}{
						"resources": map[string]interface{}{
							"requests": map[string]interface{}{"storage": "10Gi"},
						},
					},
				},
			},
		},
	}
}

var testUser = &models.User{Username: "alice", UID: 1234, GID: 5678}

func TestEffectiveNoProfileReturnsSame(t *testing.T) {
	bp := testBlueprint("")
	got, err := Effective(bp, testUser)
	if err != nil || got != bp {
		t.Fatalf("expected the same blueprint, got %p err %v", got, err)
	}
	bp = testBlueprint(Kata)
	bp.RuntimeClassName = ""
	bp.Podman.Enabled = false
	if got, _ := Effective(bp, testUser); got != bp {
		t.Fatal("disabled podman must not expand a profile")
	}
}

func TestEffectiveDoesNotMutateInput(t *testing.T) {
	bp := testBlueprint(Kata)
	if _, err := Effective(bp, testUser); err != nil {
		t.Fatal(err)
	}
	if bp.Podman.StorageImage != nil || len(bp.Podman.ExtFiles) != 0 {
		t.Fatalf("input was mutated: %+v", bp.Podman)
	}
}

func TestPrivileged(t *testing.T) {
	bp, err := Effective(testBlueprint(Privileged), testUser)
	if err != nil {
		t.Fatal(err)
	}
	s := bp.Podman.Storages["graphdb"]
	if s.Path != "/var/lib/containers/storage" || *s.FsOwnerUid != 0 || *s.FsOwnerGid != 0 {
		t.Fatalf("storage: %+v", s)
	}
	if bp.Podman.SecurityContext["privileged"] != true {
		t.Fatalf("securityContext: %v", bp.Podman.SecurityContext)
	}
	if bp.RuntimeClassName != "" || bp.Podman.StorageImage != nil {
		t.Fatal("privileged must not set kata fields")
	}
}

func TestRootless(t *testing.T) {
	bp, err := Effective(testBlueprint(Rootless), testUser)
	if err != nil {
		t.Fatal(err)
	}
	p := bp.Podman
	if p.SecurityContext["runAsUser"] != 1234 || p.SecurityContext["runAsGroup"] != 5678 {
		t.Fatalf("securityContext: %v", p.SecurityContext)
	}
	if !strings.Contains(p.ExtFiles["/etc/passwd"], "alice:x:1234:5678::/home/podman:/bin/sh\n") {
		t.Fatalf("passwd: %q", p.ExtFiles["/etc/passwd"])
	}
	if p.ExtFiles["/etc/subuid"] != "alice:2000000000:65536\n" {
		t.Fatalf("subuid: %q", p.ExtFiles["/etc/subuid"])
	}
	s := p.Storages["graphdb"]
	if s.Path != "/home/podman" || s.FsOwnerUid != nil {
		t.Fatalf("storage: %+v", s)
	}
	if _, err := Effective(testBlueprint(Rootless), nil); err == nil {
		t.Fatal("rootless without a user must fail")
	}
}

func TestKataDefaults(t *testing.T) {
	bp, err := Effective(testBlueprint(Kata), testUser)
	if err != nil {
		t.Fatal(err)
	}
	if bp.RuntimeClassName != "kata-qemu" || bp.Podman.SocketMedium != "Memory" {
		t.Fatalf("runtime %q medium %q", bp.RuntimeClassName, bp.Podman.SocketMedium)
	}
	si := bp.Podman.StorageImage
	if si.Mode != "file" || si.DiskPath != DefaultFileDiskPath || si.MountPath != DefaultStorageMountPath || si.InitImage != "registry.example/dev:1" {
		t.Fatalf("storageImage: %+v", si)
	}
	// 90% of 10Gi
	if si.Size != "9663676416" {
		t.Fatalf("size %q", si.Size)
	}
	if s := bp.Podman.Storages["graphdb"]; s.Path != DefaultFileDiskPath {
		t.Fatalf("storage path %q", s.Path)
	}
	if !strings.Contains(bp.Podman.StartupScript, "mknod -m 600 /dev/kmsg") ||
		!strings.Contains(bp.Podman.ExtFiles["/etc/containers/storage.conf"], `mountopt = "nodev"`) {
		t.Fatal("startup script or storage.conf missing")
	}
}

func TestKataRequiresRuntimeClass(t *testing.T) {
	in := testBlueprint(Kata)
	in.RuntimeClassName = ""
	if _, err := Effective(in, testUser); !errors.Is(err, ErrKataRuntimeClass) {
		t.Fatalf("err = %v, want ErrKataRuntimeClass", err)
	}
}

func TestKataBlockMode(t *testing.T) {
	in := testBlueprint(Kata)
	in.Podman.StorageImage = &models.PodmanStorageImage{Mode: "block"}
	bp, _ := Effective(in, testUser)
	si := bp.Podman.StorageImage
	if si.DiskPath != DefaultBlockDevicePath || si.Size != "" {
		t.Fatalf("storageImage: %+v", si)
	}
}

func TestExplicitValuesWin(t *testing.T) {
	in := testBlueprint(Kata)
	in.RuntimeClassName = "kata-clh"
	in.Podman.StartupScript = "echo mine"
	in.Podman.SocketMedium = ""
	in.Podman.ExtFiles = map[string]string{"/etc/containers/storage.conf": "mine"}
	in.Podman.StorageImage = &models.PodmanStorageImage{Size: "5Gi", InitImage: "mkfs:1"}
	s := in.Podman.Storages["graphdb"]
	s.Path = "/custom"
	in.Podman.Storages["graphdb"] = s

	bp, _ := Effective(in, testUser)
	p := bp.Podman
	if bp.RuntimeClassName != "kata-clh" || p.StartupScript != "echo mine" ||
		p.ExtFiles["/etc/containers/storage.conf"] != "mine" ||
		p.StorageImage.Size != "5Gi" || p.StorageImage.InitImage != "mkfs:1" || p.Storages["graphdb"].Path != "/custom" {
		t.Fatalf("explicit values overridden: %+v", p)
	}
}

// A blueprint inherits podman.securityContext from its base template, so the
// profile's keys must win over it while unrelated keys survive.
func TestSecurityContextProfileWinsOverInherited(t *testing.T) {
	in := testBlueprint(Kata)
	in.Podman.SecurityContext = map[string]interface{}{
		"runAsUser":       1000,
		"runAsGroup":      1000,
		"appArmorProfile": map[string]interface{}{"type": "Unconfined"},
		"capabilities":    map[string]interface{}{"add": []interface{}{"SYS_ADMIN"}},
	}
	bp, _ := Effective(in, testUser)
	sc := bp.Podman.SecurityContext
	if sc["privileged"] != true || sc["runAsUser"] != 0 || sc["runAsGroup"] != 0 {
		t.Fatalf("profile keys did not win: %v", sc)
	}
	if sc["capabilities"] == nil {
		t.Fatalf("unrelated key dropped: %v", sc)
	}
}

func TestMissingStorageNotCreated(t *testing.T) {
	in := testBlueprint(Privileged)
	in.Podman.Storages = nil
	bp, _ := Effective(in, testUser)
	if len(bp.Podman.Storages) != 0 {
		t.Fatalf("profile created a storage: %+v", bp.Podman.Storages)
	}
}

func TestKataEmptyDirSizeFromSizeLimit(t *testing.T) {
	in := testBlueprint(Kata)
	in.Podman.Storages = map[string]models.Storage{
		"graphdb": {Enabled: true, Type: "emptyDir", SizeLimit: "10Gi"},
	}
	bp, _ := Effective(in, testUser)
	if got := bp.Podman.StorageImage.Size; got != "9663676416" {
		t.Fatalf("size %q, want 90%% of the sizeLimit", got)
	}
}

func TestKataCacheTrim(t *testing.T) {
	bp, _ := Effective(testBlueprint(Kata), testUser)
	if n := bp.Podman.CacheTrimSeconds; n == nil || *n != DefaultCacheTrimSeconds {
		t.Fatalf("default cacheTrimSeconds = %v, want %d", n, DefaultCacheTrimSeconds)
	}

	for name, set := range map[string]int{"explicit interval": 90, "disabled": 0} {
		in := testBlueprint(Kata)
		in.Podman.CacheTrimSeconds = &set
		got, _ := Effective(in, testUser)
		if n := got.Podman.CacheTrimSeconds; n == nil || *n != set {
			t.Fatalf("%s: cacheTrimSeconds = %v, want %d", name, n, set)
		}
	}

	if bp, _ := Effective(testBlueprint(Privileged), testUser); bp.Podman.CacheTrimSeconds != nil {
		t.Fatal("only the kata profile trims the cache")
	}
}
