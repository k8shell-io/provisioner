// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/k8shell-io/common/pkg/models"
)

// fakeOrgStore is an in-memory OrgBlueprintStore for tests.
type fakeOrgStore struct {
	mu         sync.Mutex
	blueprints []*models.OrgBlueprint
}

func (f *fakeOrgStore) ListAllBlueprints() ([]*models.OrgBlueprint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*models.OrgBlueprint, len(f.blueprints))
	copy(out, f.blueprints)
	return out, nil
}

func (f *fakeOrgStore) set(blueprints []*models.OrgBlueprint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blueprints = blueprints
}

func newTestManagerWithOrgStore(t *testing.T, files map[string]string, store OrgBlueprintStore) *BlueprintManager {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write fixture %s: %v", name, err)
		}
	}

	bm, err := NewBlueprintManager(LoadOptions{Dir: dir, EnableWatch: true, OrgStore: store})
	if err != nil {
		t.Fatalf("failed to create blueprint manager: %v", err)
	}
	return bm
}

func scopeForOrg(org string) *BlueprintScope {
	s := TestScope()
	s.User.Organization = org
	return s
}

// requiredBlueprintFields holds every field models.Blueprint.Validate
// requires besides name/image, which callers supply themselves.
const requiredBlueprintFields = `
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 500m
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
`

func TestOrgBlueprintOverridesFileBlueprintForItsOrg(t *testing.T) {
	store := &fakeOrgStore{}
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"dev.yaml": "name: dev\ndescription: test blueprint\nimage: myimage:latest\n" + requiredBlueprintFields,
	}, store)

	store.set([]*models.OrgBlueprint{
		{Org: "acme", Name: "dev", YAML: []byte("name: dev\ndescription: test blueprint\nimage: acme-image:latest\n" + requiredBlueprintFields)},
	})
	if err := bm.ReloadOrgBlueprints(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	// A user in org "acme" gets the org-scoped blueprint.
	bp, err := bm.GetBlueprint("dev", scopeForOrg("acme"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bp.Image != "acme-image:latest" {
		t.Fatalf("expected org-scoped image, got %q", bp.Image)
	}

	// A user in a different (or no) org still gets the file-based blueprint.
	bp, err = bm.GetBlueprint("dev", scopeForOrg("other-org"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bp.Image != "myimage:latest" {
		t.Fatalf("expected file-based image, got %q", bp.Image)
	}
}

func TestOrgBlueprintInheritsFileTemplate(t *testing.T) {
	store := &fakeOrgStore{}
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"base.yaml": "name: base\nisTemplate: true\ndescription: test template\nimage: base-image:latest\n" + requiredBlueprintFields,
	}, store)

	store.set([]*models.OrgBlueprint{
		{Org: "acme", Name: "custom", YAML: []byte("name: custom\ntemplate: base\ndescription: test org blueprint\n")},
	})
	if err := bm.ReloadOrgBlueprints(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	bp, err := bm.GetBlueprint("custom", scopeForOrg("acme"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bp.Image != "base-image:latest" {
		t.Fatalf("expected image inherited from template, got %q", bp.Image)
	}
}

func TestGetBlueprintsSummaryIncludesOrgBlueprints(t *testing.T) {
	store := &fakeOrgStore{}
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"dev.yaml": "name: dev\ndescription: test blueprint\nimage: myimage:latest\n" + requiredBlueprintFields,
	}, store)

	store.set([]*models.OrgBlueprint{
		{Org: "acme", Name: "custom", YAML: []byte("name: custom\ndescription: test org blueprint\nimage: custom-image:latest\n" + requiredBlueprintFields)},
	})
	if err := bm.ReloadOrgBlueprints(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	byName := map[string]*models.BlueprintSummary{}
	for _, s := range bm.GetBlueprintsSummary() {
		byName[s.Name] = s
	}

	dev, ok := byName["dev"]
	if !ok {
		t.Fatalf("expected the file-based 'dev' summary, got %+v", byName)
	}
	if dev.Org != "" || !dev.IsGlobal {
		t.Fatalf("expected 'dev' to be a global blueprint, got %+v", dev)
	}

	custom, ok := byName["custom"]
	if !ok {
		t.Fatalf("expected the org-scoped 'custom' summary, got %+v", byName)
	}
	if custom.Org != "acme" || custom.IsGlobal {
		t.Fatalf("expected 'custom' to be scoped to org 'acme', got %+v", custom)
	}
}

func TestGetBlueprintsSummaryReportsTimestamps(t *testing.T) {
	store := &fakeOrgStore{}
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"dev.yaml": "name: dev\ndescription: test blueprint\nimage: myimage:latest\n" + requiredBlueprintFields,
	}, store)

	fileMTime := bm.rawBlueprints["dev"].UpdatedAt
	if fileMTime.IsZero() {
		t.Fatalf("expected file-based blueprint to carry a non-zero mtime")
	}

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	store.set([]*models.OrgBlueprint{
		{
			Org: "acme", Name: "custom",
			YAML:      []byte("name: custom\ndescription: test org blueprint\nimage: custom-image:latest\n" + requiredBlueprintFields),
			CreatedAt: created, UpdatedAt: updated,
		},
	})
	if err := bm.ReloadOrgBlueprints(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	byName := map[string]*models.BlueprintSummary{}
	for _, s := range bm.GetBlueprintsSummary() {
		byName[s.Name] = s
	}

	dev := byName["dev"]
	if dev == nil {
		t.Fatalf("missing 'dev' summary")
	}
	if !dev.CreatedAt.Equal(dev.UpdatedAt) {
		t.Fatalf("file blueprint created/updated should match, got %v / %v", dev.CreatedAt, dev.UpdatedAt)
	}
	if dev.CreatedAt.IsZero() {
		t.Fatalf("file blueprint timestamps should be the file mtime, got zero")
	}

	custom := byName["custom"]
	if custom == nil {
		t.Fatalf("missing 'custom' summary")
	}
	if !custom.CreatedAt.Equal(created) || !custom.UpdatedAt.Equal(updated) {
		t.Fatalf("org blueprint should carry the db row timestamps, got %v / %v", custom.CreatedAt, custom.UpdatedAt)
	}
}

func TestOrgBlueprintNotFoundWithoutStore(t *testing.T) {
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"dev.yaml": "name: dev\ndescription: test blueprint\nimage: myimage:latest\n" + requiredBlueprintFields,
	}, nil)

	bp, err := bm.GetBlueprint("dev", scopeForOrg("acme"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bp.Image != "myimage:latest" {
		t.Fatalf("expected file-based image when no org store is configured, got %q", bp.Image)
	}
}
