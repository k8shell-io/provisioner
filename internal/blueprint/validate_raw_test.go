// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"os"
	"path/filepath"
	"testing"
)

func assertAllFieldsSet(t *testing.T, issues []ValidationIssue) {
	t.Helper()
	for _, issue := range issues {
		if issue.Field == "" {
			t.Errorf("issue has no Field: %+v", issue)
		}
	}
}

func newTestManager(t *testing.T, files map[string]string) *BlueprintManager {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write fixture %s: %v", name, err)
		}
	}

	bm, err := NewBlueprintManager(LoadOptions{Dir: dir, EnableWatch: true})
	if err != nil {
		t.Fatalf("failed to create blueprint manager: %v", err)
	}
	return bm
}

func TestValidateRawBlueprintValid(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
image: myimage:latest
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 500m
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected no issues, got %+v", issues)
	}
}

func TestValidateRawBlueprintMissingRequiredField(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
k8shelld:
  image: k8shelld-image:latest
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) == 0 {
		t.Fatalf("expected at least one issue for missing 'image' field")
	}
	assertAllFieldsSet(t, issues)

	found := false
	for _, issue := range issues {
		if issue.Field == "image" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an issue for the image field, got %+v", issues)
	}
}

func TestValidateRawBlueprintSyntaxError(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte("name: [unterminated\n")

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected exactly one syntax issue, got %+v", issues)
	}
	assertAllFieldsSet(t, issues)
}

func TestValidateRawBlueprintUnknownTemplate(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
template: does-not-exist
image: myimage:latest
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0].Field != "template" {
		t.Fatalf("expected a single 'template' issue, got %+v", issues)
	}
}

// TestValidateRawBlueprintDisplayCELPrefix reproduces round-tripping a
// blueprint fetched via GetBlueprint (which renders CEL expressions as
// "!cel:"-prefixed plain strings) straight back into ValidateBlueprint: the
// prefixed strings must be evaluated as CEL, not treated as literal hostname
// values.
func TestValidateRawBlueprintDisplayCELPrefix(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
image: myimage:latest
hostname: "!cel:user.username + '-' + metadata.name"
subdomain: "!cel:user.organization"
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 500m
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected no issues once CEL expressions are evaluated, got %+v", issues)
	}
}

// TestValidateRawBlueprintInvalidResourceQuantity reproduces a bogus CPU /
// memory value passing validation because models.Resources only enforces
// "required" (non-empty), not that the string is a valid Kubernetes quantity.
func TestValidateRawBlueprintInvalidResourceQuantity(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
image: myimage:latest
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 4ddsfsdf
  memory: 4Gidfdsfsdf
podman:
  resources:
    cpu: 500m
    memory: 512Mi
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues for the malformed cpu/memory quantities, got %+v", issues)
	}
	assertAllFieldsSet(t, issues)
}

// TestValidateRawBlueprintInvalidEnvName reproduces an env var key with a
// space ("TEST X") passing validation because models.Blueprint.Env carries
// no validate tag on its map keys.
func TestValidateRawBlueprintInvalidEnvName(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
image: myimage:latest
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 500m
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
env:
  "TEST X": client
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue for the invalid env name, got %+v", issues)
	}
	assertAllFieldsSet(t, issues)
}

// TestValidateRawBlueprintReportsFieldForEveryIssue reproduces the exact
// combination reported: a network.allowEgressToCIDRs entry that fails go-
// playground's "cidr" validation (which always sets Field) alongside an
// invalid env name (a custom check), confirming both come back with Field set.
func TestValidateRawBlueprintReportsFieldForEveryIssue(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
image: myimage:latest
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 500m
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
network:
  allowEgressToCIDRs:
    - "l;;l"
env:
  " f fff ": client
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues (bad CIDR, bad env name), got %+v", issues)
	}
	assertAllFieldsSet(t, issues)
}

// TestValidateRawBlueprintReportsAllIssues ensures an unknown field doesn't
// hide unrelated problems: a decode error (unknown field) still leaves the
// rest of the document decoded, so validation must keep running instead of
// returning only the first issue found.
func TestValidateRawBlueprintReportsAllIssues(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
bogusField: true
image: myimage:latest
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 4ddsfsdf
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
env:
  "TEST X": client
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("expected 3 independent issues (unknown field, bad cpu quantity, bad env name), got %+v", issues)
	}
	assertAllFieldsSet(t, issues)
}

// TestValidateRawBlueprintFieldPathFormat pins down the exact field-path
// format the frontend relies on to attach an error to the right form field:
// dot-separated, fully qualified from the blueprint root, lowerCamelCase
// matching JSON tags, and map/slice entries as unquoted [key] appended
// directly to the field name (no dot before the bracket).
func TestValidateRawBlueprintFieldPathFormat(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
image: myimage:latest
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 500m
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
network:
  allowEgressToCIDRs:
    - "l;;l"
storages:
  data:
    enabled: true
    sizeLimit: bogus
    type: emptyDir
env:
  " f fff ": client
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]bool{
		"network.allowEgressToCIDRs[0]": false,
		"storages[data].sizeLimit":      false,
		"env[ f fff ]":                  false,
	}
	for _, issue := range issues {
		if _, ok := want[issue.Field]; ok {
			want[issue.Field] = true
		}
	}
	for field, found := range want {
		if !found {
			t.Errorf("expected an issue with field %q, got %+v", field, issues)
		}
	}
}

// TestValidateRawBlueprintCELErrorField reproduces a CEL expression
// referencing an undeclared function: the failure must point at the actual
// field the expression lives on (e.g. "k8shelld.image"), not the generic
// "cel" sentinel.
func TestValidateRawBlueprintCELErrorField(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
image: myimage:latest
k8shelld:
  image: !cel envx("DEFAULT_K8SHELLD_IMAGE")
resources:
  cpu: 500m
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected exactly one issue, got %+v", issues)
	}
	if issues[0].Field != "k8shelld.image" {
		t.Fatalf("expected field %q, got %+v", "k8shelld.image", issues[0])
	}
}

func TestValidateRawBlueprintWithTemplate(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": `
name: base
isTemplate: true
image: base-image:latest
k8shelld:
  image: k8shelld-image:latest
resources:
  cpu: 500m
  memory: 512Mi
podman:
  resources:
    cpu: 500m
    memory: 512Mi
`,
	})

	yaml := []byte(`
name: child
template: base
`)

	issues, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected no issues inheriting from a valid template, got %+v", issues)
	}
}
