// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"testing"

	"github.com/k8shell-io/provisioner/internal/podmanprofile"
)

// TestShippedKataBlueprint loads config/blueprints (which validates every
// blueprint in it) and checks the kata blueprints expand to a Kata workspace.
func TestShippedKataBlueprint(t *testing.T) {
	t.Setenv("WORKSPACE_STORAGE_CLASS", "test")
	t.Setenv("KATA_RUNTIME_CLASS", "kata-qemu")
	bm, err := NewBlueprintManager(LoadOptions{Dir: "../../config/blueprints", EnableWatch: true})
	if err != nil {
		t.Fatalf("load shipped blueprints: %v", err)
	}

	scope := TestScope()
	bp, err := bm.GetBlueprint("kata-docker", scope)
	if err != nil {
		t.Fatalf("get kata-docker: %v", err)
	}
	eff, err := podmanprofile.Effective(bp, scope.User)
	if err != nil {
		t.Fatal(err)
	}
	si := eff.Podman.StorageImage
	if eff.RuntimeClassName != "kata-qemu" || si == nil || si.Mode != "file" || si.Size != "14495514624" {
		t.Fatalf("effective kata blueprint: runtimeClass=%q storageImage=%+v", eff.RuntimeClassName, si)
	}
	// base.yaml sets a non-privileged podman securityContext that kata-docker
	// inherits; the profile must still make the sidecar privileged.
	if sc := eff.Podman.SecurityContext; sc["privileged"] != true || sc["runAsUser"] != 0 {
		t.Fatalf("kata podman securityContext = %v, want privileged root", sc)
	}
	if bp.Env["DOCKER_HOST"] == "" {
		t.Fatal("sample env missing")
	}
}
