// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

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
		"dev.yaml": "name: dev\nimage: myimage:latest\n" + requiredBlueprintFields,
	}, store)

	store.set([]*models.OrgBlueprint{
		{Org: "acme", Name: "dev", YAML: []byte("name: dev\nimage: acme-image:latest\n" + requiredBlueprintFields)},
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
		"base.yaml": "name: base\nisTemplate: true\nimage: base-image:latest\n" + requiredBlueprintFields,
	}, store)

	store.set([]*models.OrgBlueprint{
		{Org: "acme", Name: "custom", YAML: []byte("name: custom\ntemplate: base\n")},
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

func TestGetBlueprintsSummaryExcludesOrgBlueprints(t *testing.T) {
	store := &fakeOrgStore{}
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"dev.yaml": "name: dev\nimage: myimage:latest\n" + requiredBlueprintFields,
	}, store)

	store.set([]*models.OrgBlueprint{
		{Org: "acme", Name: "custom", YAML: []byte("name: custom\nimage: custom-image:latest\n" + requiredBlueprintFields)},
	})
	if err := bm.ReloadOrgBlueprints(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	summaries := bm.GetBlueprintsSummary()
	for _, s := range summaries {
		if s.Name == "custom" {
			t.Fatalf("org blueprint leaked into global summary: %+v", s)
		}
	}
	if len(summaries) != 1 || summaries[0].Name != "dev" {
		t.Fatalf("expected only the file-based 'dev' summary, got %+v", summaries)
	}
}

func TestOrgBlueprintNotFoundWithoutStore(t *testing.T) {
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"dev.yaml": "name: dev\nimage: myimage:latest\n" + requiredBlueprintFields,
	}, nil)

	bp, err := bm.GetBlueprint("dev", scopeForOrg("acme"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bp.Image != "myimage:latest" {
		t.Fatalf("expected file-based image when no org store is configured, got %q", bp.Image)
	}
}
