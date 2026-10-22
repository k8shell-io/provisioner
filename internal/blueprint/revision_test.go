// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"testing"

	"github.com/k8shell-io/common/pkg/models"
	"gopkg.in/yaml.v3"
)

func revisionOf(t *testing.T, src string) string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return computeRevision(&doc)
}

func TestComputeRevisionIsCanonical(t *testing.T) {
	base := revisionOf(t, "name: dev\nimage: a:1\nenv:\n  A: x\n  B: y\n")
	if len(base) != revisionLen {
		t.Fatalf("expected %d-char revision, got %q", revisionLen, base)
	}

	same := map[string]string{
		"key order":   "env:\n  B: y\n  A: x\nimage: a:1\nname: dev\n",
		"comments":    "# header\nname: dev # inline\nimage: a:1\nenv:\n  A: x\n  B: y\n",
		"quoting":     "name: \"dev\"\nimage: 'a:1'\nenv:\n  A: x\n  B: y\n",
		"description": "name: dev\ndescription: anything\nimage: a:1\nenv:\n  A: x\n  B: y\n",
		"isTemplate":  "name: dev\nisTemplate: true\nimage: a:1\nenv:\n  A: x\n  B: y\n",
	}
	for what, src := range same {
		if got := revisionOf(t, src); got != base {
			t.Errorf("%s: expected revision to be unchanged (%s), got %s", what, base, got)
		}
	}

	different := map[string]string{
		"value":         "name: dev\nimage: a:2\nenv:\n  A: x\n  B: y\n",
		"cel tag":       "name: dev\nimage: !cel a:1\nenv:\n  A: x\n  B: y\n",
		"scalar type":   "name: dev\nimage: a:1\nenv:\n  A: x\n  B: y\nport: \"1\"\n",
		"nested desc":   "name: dev\nimage: a:1\nenv:\n  A: x\n  B: y\n  description: z\n",
		"missing field": "name: dev\nimage: a:1\nenv:\n  A: x\n",
	}
	for what, src := range different {
		if got := revisionOf(t, src); got == base {
			t.Errorf("%s: expected revision to change, still %s", what, got)
		}
	}

	if revisionOf(t, "l: [a, b]\n") == revisionOf(t, "l: [b, a]\n") {
		t.Errorf("expected sequence order to affect the revision")
	}
	if revisionOf(t, "port: 1\n") == revisionOf(t, "port: \"1\"\n") {
		t.Errorf("expected int and string scalars to hash differently")
	}
}

// TestRevisionPropagatesThroughInheritance checks that a change to a root
// template changes the revision of every descendant that actually inherits
// the changed field, and of no descendant that overrides it.
func TestRevisionPropagatesThroughInheritance(t *testing.T) {
	load := func(baseImage, baseSplash string) *BlueprintManager {
		return newTestManager(t, map[string]string{
			"base.yaml": "name: base\nisTemplate: true\ndescription: base\nimage: " + baseImage +
				"\nsplash: " + baseSplash + "\n" + requiredBlueprintFields,
			"mid.yaml":  "name: mid\nisTemplate: true\ntemplate: base\ndescription: mid\n",
			"leaf.yaml": "name: leaf\ntemplate: mid\ndescription: leaf\nsplash: leaf-splash\n",
		})
	}
	revs := func(bm *BlueprintManager) map[string]string {
		out := map[string]string{}
		for _, n := range []string{"base", "mid", "leaf"} {
			out[n] = bm.GetBlueprintRevision("", n)
			if out[n] == "" {
				t.Fatalf("missing revision for %s", n)
			}
		}
		return out
	}

	orig := revs(load("img:1", "base-splash"))

	// A field every descendant inherits: the whole chain changes.
	imageChanged := revs(load("img:2", "base-splash"))
	for _, n := range []string{"base", "mid", "leaf"} {
		if imageChanged[n] == orig[n] {
			t.Errorf("image change: expected %s revision to change", n)
		}
	}

	// A field leaf overrides: base and mid change, leaf does not.
	splashChanged := revs(load("img:1", "other-splash"))
	for _, n := range []string{"base", "mid"} {
		if splashChanged[n] == orig[n] {
			t.Errorf("splash change: expected %s revision to change", n)
		}
	}
	if splashChanged["leaf"] != orig["leaf"] {
		t.Errorf("splash change: expected leaf revision to stay %s, got %s", orig["leaf"], splashChanged["leaf"])
	}
}

func TestOrgBlueprintRevisionFollowsFileTemplate(t *testing.T) {
	load := func(baseImage string) *BlueprintManager {
		store := &fakeOrgStore{}
		bm := newTestManagerWithOrgStore(t, map[string]string{
			"base.yaml": "name: base\nisTemplate: true\ndescription: base\nimage: " + baseImage + "\n" + requiredBlueprintFields,
		}, store)
		store.set([]*models.OrgBlueprint{
			{Org: "acme", Name: "custom", YAML: []byte("name: custom\ntemplate: base\ndescription: org\n")},
		})
		if err := bm.ReloadOrgBlueprints(); err != nil {
			t.Fatalf("reload failed: %v", err)
		}
		return bm
	}

	a := load("img:1").GetBlueprintRevision("acme", "custom")
	b := load("img:2").GetBlueprintRevision("acme", "custom")
	if a == "" || b == "" {
		t.Fatalf("expected org blueprint revisions, got %q and %q", a, b)
	}
	if a == b {
		t.Fatalf("expected org blueprint revision to change with its file template, still %s", a)
	}
}

// TestRevisionIsConsistentAcrossReadPaths checks that the summary,
// GetRawBlueprintScoped, GetBlueprintRevision and OrgBlueprintRevision all
// report the same revision, so a revision read from any of them can be used
// as UpdateBlueprintRequest.expected_revision.
func TestRevisionIsConsistentAcrossReadPaths(t *testing.T) {
	store := &fakeOrgStore{}
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"base.yaml": "name: base\nisTemplate: true\ndescription: base\nimage: img:1\n" + requiredBlueprintFields,
	}, store)
	row := &models.OrgBlueprint{Org: "acme", Name: "custom", YAML: []byte("name: custom\ntemplate: base\ndescription: org\n")}
	store.set([]*models.OrgBlueprint{row})
	if err := bm.ReloadOrgBlueprints(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	sums, err := bm.GetBlueprintsSummary()
	if err != nil {
		t.Fatalf("GetBlueprintsSummary: %v", err)
	}
	fromSummary := map[string]string{}
	for _, s := range sums {
		fromSummary[s.Org+"/"+s.Name] = s.Revision
	}

	for _, tc := range []struct{ org, name string }{{"", "base"}, {"acme", "custom"}} {
		key := tc.org + "/" + tc.name
		want := bm.GetBlueprintRevision(tc.org, tc.name)
		if want == "" {
			t.Fatalf("%s: missing revision", key)
		}
		if fromSummary[key] != want {
			t.Errorf("%s: summary revision %q, want %q", key, fromSummary[key], want)
		}
		_, _, _, scoped, err := bm.GetRawBlueprintScoped(tc.org, tc.name)
		if err != nil {
			t.Fatalf("%s: GetRawBlueprintScoped: %v", key, err)
		}
		if scoped != want {
			t.Errorf("%s: GetRawBlueprintScoped revision %q, want %q", key, scoped, want)
		}
	}

	rev, err := bm.OrgBlueprintRevision(row)
	if err != nil {
		t.Fatalf("OrgBlueprintRevision: %v", err)
	}
	if rev != bm.GetBlueprintRevision("acme", "custom") {
		t.Errorf("OrgBlueprintRevision %q differs from GetBlueprintRevision", rev)
	}

	edited := &models.OrgBlueprint{Org: "acme", Name: "custom",
		YAML: []byte("name: custom\ntemplate: base\ndescription: org\nimage: img:2\n")}
	if editedRev, _ := bm.OrgBlueprintRevision(edited); editedRev == rev {
		t.Errorf("expected an edit to change OrgBlueprintRevision, still %s", rev)
	}
}
