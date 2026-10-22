// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"strings"
	"testing"

	"github.com/k8shell-io/common/pkg/models"
)

func TestBlueprintFromValuesDropsNonBlueprintKeys(t *testing.T) {
	values := map[string]interface{}{
		"name":          "dev",
		"image":         "img:1",
		"__jobid__":     "job-1",
		"__pat__":       "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"__registry__":  map[string]interface{}{"password": "x"},
		"initScripts":   []interface{}{map[string]interface{}{"name": "a", "script": "echo", "__file": "00-a.sh"}},
		"unknownByHelm": "x",
	}
	bp, err := BlueprintFromValues(values)
	if err != nil {
		t.Fatal(err)
	}
	if bp.Name != "dev" || bp.Image != "img:1" || len(bp.InitScripts) != 1 {
		t.Fatalf("unexpected blueprint: %+v", bp)
	}
	d, err := DiffBlueprints(bp, bp, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"job-1", "ghp_", "00-a.sh", "unknownByHelm"} {
		if strings.Contains(d.WorkspaceYAML, leak) {
			t.Errorf("rendered YAML leaks %q:\n%s", leak, d.WorkspaceYAML)
		}
	}
}

func TestDiffBlueprintsStripsWorkspaceAdditions(t *testing.T) {
	current := &models.Blueprint{Name: "dev", Image: "img:1", Env: map[string]string{"A": "1"}}
	deployed := &models.Blueprint{
		Name: "dev", Image: "img:1",
		Hostname: "my-ws", Subdomain: "acme",
		Metadata: models.BlueprintMetadata{Name: "dev"},
		Env: map[string]string{
			"A": "1", "PROVISIONER_VERSION": "v1", "PROVISION_MODE": "standalone",
			"GIT_REPONAME": "x", "MY_USER_VAR": "u",
		},
	}
	d, err := DiffBlueprints(deployed, current, DiffOptions{
		WorkspaceName: "my-ws", Organization: "acme", UserEnvKeys: []string{"MY_USER_VAR"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Identical || d.Added+d.Removed+d.Changed != 0 {
		t.Fatalf("expected identical, got +%d -%d ~%d:\n%s\n---\n%s",
			d.Added, d.Removed, d.Changed, d.WorkspaceYAML, d.BlueprintYAML)
	}
}

func TestDiffBlueprintsKeepsRealDifferences(t *testing.T) {
	deployed := &models.Blueprint{Name: "dev", Image: "img:1", Hostname: "custom"}
	current := &models.Blueprint{Name: "dev", Image: "img:2", Hostname: "custom"}
	d, err := DiffBlueprints(deployed, current, DiffOptions{WorkspaceName: "ws", Organization: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Identical || d.Changed != 1 {
		t.Fatalf("want one changed row, got identical=%v +%d -%d ~%d", d.Identical, d.Added, d.Removed, d.Changed)
	}
	for _, r := range d.Rows {
		if r.Op == DiffChanged &&
			(!strings.Contains(r.LeftText, "img:1") || !strings.Contains(r.RightText, "img:2")) {
			t.Errorf("unexpected changed row: %+v", r)
		}
	}
}

func TestDiffBlueprintsRedactsSecrets(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----"
	mk := func(token, pass string) *models.Blueprint {
		return &models.Blueprint{
			Name: "dev", Image: "img",
			Env: map[string]string{"API_TOKEN": token, "DB_PASSWORD": pass, "PLAIN": "visible"},
			InitScripts: []models.InitScript{
				{Name: "s", Script: "echo ghp_abcdefghijklmnopqrstuvwxyz0123456789\n" + pem},
			},
		}
	}
	d, err := DiffBlueprints(mk("tok-1", "pw"), mk("tok-2", "pw"), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, y := range []string{d.WorkspaceYAML, d.BlueprintYAML} {
		for _, leak := range []string{"tok-1", "tok-2", "ghp_", "BEGIN RSA", "abc\n"} {
			if strings.Contains(y, leak) {
				t.Errorf("YAML leaks %q:\n%s", leak, y)
			}
		}
		if !strings.Contains(y, "visible") {
			t.Errorf("non-secret value was redacted:\n%s", y)
		}
	}
	// A changed secret still registers as a change, an unchanged one doesn't.
	if d.Changed != 1 {
		t.Fatalf("want exactly the changed token to differ, got ~%d:\n%s\n---\n%s", d.Changed, d.WorkspaceYAML, d.BlueprintYAML)
	}
}

func TestDiffLines(t *testing.T) {
	rows := DiffLines("a\nb\nc\n", "a\nB\nc\nd\n")
	want := []DiffRow{
		{Op: DiffEqual, LeftLine: 1, LeftText: "a", RightLine: 1, RightText: "a"},
		{Op: DiffChanged, LeftLine: 2, LeftText: "b", RightLine: 2, RightText: "B"},
		{Op: DiffEqual, LeftLine: 3, LeftText: "c", RightLine: 3, RightText: "c"},
		{Op: DiffAdded, RightLine: 4, RightText: "d"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	if got := DiffLines("", ""); len(got) != 0 {
		t.Errorf("empty diff produced rows: %+v", got)
	}
	if got := DiffLines("x\n", ""); len(got) != 1 || got[0].Op != DiffRemoved {
		t.Errorf("want one removed row, got %+v", got)
	}
}

func TestAppliedPatchesFromValues(t *testing.T) {
	if got, err := AppliedPatchesFromValues(map[string]interface{}{}); err != nil || got != nil {
		t.Fatalf("no stored file: got %v, %v", got, err)
	}
	doc := "# inheritance: base -> dev\nmetadata: {}\nblueprint: {}\nobligations:\n  /resources/memory: 1Gi\n  /resources/cpu: \"1\"\n"
	got, err := AppliedPatchesFromValues(map[string]interface{}{"__blueprintyaml__": doc})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "/resources/cpu" || got[0].Value != "1" ||
		got[1].Path != "/resources/memory" || got[1].Value != "1Gi" {
		t.Errorf("unexpected patches: %+v", got)
	}
}
