// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"strings"
	"testing"

	"github.com/k8shell-io/common/pkg/models"
)

func kataBlueprint(volumeMode string, si *models.PodmanStorageImage) *models.Blueprint {
	claim := map[string]interface{}{
		"resources": map[string]interface{}{"requests": map[string]interface{}{"storage": "10Gi"}},
	}
	if volumeMode != "" {
		claim["volumeMode"] = volumeMode
	}
	return &models.Blueprint{
		RuntimeClassName: "kata-qemu",
		Image:            "dev:1",
		Podman: models.Podman{
			Enabled:      true,
			Profile:      "kata",
			StorageImage: si,
			Storages:     map[string]models.Storage{"graphdb": {Enabled: true, ClaimSpec: claim}},
		},
	}
}

func issues(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Error() + "\n")
	}
	return b.String()
}

func TestValidatePodmanProfile(t *testing.T) {
	tests := []struct {
		name string
		bp   *models.Blueprint
		want string // substring of the expected error, "" for none
	}{
		{"kata file default", kataBlueprint("", nil), ""},
		{"kata block ok", kataBlueprint("Block", &models.PodmanStorageImage{Mode: "block"}), ""},
		{"block needs Block claim", kataBlueprint("", &models.PodmanStorageImage{Mode: "block"}), "needs podman.storages[graphdb].claimSpec.volumeMode Block"},
		{"file rejects Block claim", kataBlueprint("Block", nil), "needs a Filesystem claim"},
		{"size not below claim", kataBlueprint("", &models.PodmanStorageImage{Size: "10Gi"}), "must be smaller"},
		{"size invalid", kataBlueprint("", &models.PodmanStorageImage{Size: "30Gx"}), "not a valid quantity"},
		{"unknown profile", func() *models.Blueprint { b := kataBlueprint("", nil); b.Podman.Profile = "x"; return b }(), "unknown profile"},
		{"storage missing", func() *models.Blueprint {
			b := kataBlueprint("", &models.PodmanStorageImage{Storage: "nope"})
			return b
		}(), "not an enabled podman storage"},
		{"init container storage missing", func() *models.Blueprint {
			b := kataBlueprint("", nil)
			b.Podman.InitContainers = []models.PodmanInitContainer{{Name: "x", Image: "i", Command: []string{"true"}, Storages: []string{"nope"}}}
			return b
		}(), "initContainers[0]"},
		{"kata without runtimeClassName", func() *models.Blueprint { b := kataBlueprint("", nil); b.RuntimeClassName = ""; return b }(), "requires runtimeClassName"},
		{"kata emptyDir file", emptyDirKata("file", "20Gi", ""), ""},
		{"kata emptyDir needs a size", emptyDirKata("file", "", ""), "podman.storageImage.size is required"},
		{"kata emptyDir size not below limit", emptyDirKata("file", "20Gi", "20Gi"), "must be smaller"},
		{"block rejects emptyDir", emptyDirKata("block", "20Gi", ""), "needs a persistent volume claim"},
		{"cache trim too short", withTrim(kataBlueprint("", nil), 10), "must be 0 (off) or at least 30"},
		{"cache trim off", withTrim(kataBlueprint("", nil), 0), ""},
		{"cache trim ok", withTrim(kataBlueprint("", nil), 60), ""},
		{"cache trim needs runtime class", func() *models.Blueprint {
			b := withTrim(kataBlueprint("", nil), 60)
			b.Podman.Profile = ""
			b.RuntimeClassName = ""
			return b
		}(), "needs runtimeClassName"},
		{"podman disabled", func() *models.Blueprint { b := kataBlueprint("Block", nil); b.Podman.Enabled = false; return b }(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := issues(validatePodmanProfile(tt.bp))
			if tt.want == "" && got != "" {
				t.Fatalf("unexpected errors:\n%s", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("want %q in:\n%s", tt.want, got)
			}
		})
	}
}

// emptyDirKata is a kata blueprint whose graphdb storage is an emptyDir with
// the given sizeLimit; size is podman.storageImage.size ("" to leave it unset).
func emptyDirKata(mode, sizeLimit, size string) *models.Blueprint {
	bp := kataBlueprint("", &models.PodmanStorageImage{Mode: mode, Size: size})
	bp.Podman.Storages = map[string]models.Storage{
		"graphdb": {Enabled: true, Type: "emptyDir", SizeLimit: sizeLimit},
	}
	return bp
}

func withTrim(bp *models.Blueprint, seconds int) *models.Blueprint {
	bp.Podman.CacheTrimSeconds = &seconds
	return bp
}
