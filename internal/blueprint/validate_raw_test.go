// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"os"
	"path/filepath"
	"strings"
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
description: A test blueprint
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0].Field != "template" {
		t.Fatalf("expected a single 'template' issue, got %+v", issues)
	}
}

// TestValidateRawBlueprintResolvedNilWhenNothingCouldBeMerged covers the two
// cases where ValidateRawBlueprint's second return value must stay nil:
// unparseable YAML, and a `template:` that doesn't resolve. In both cases
// there is no merged document to hand back.
func TestValidateRawBlueprintResolvedNilWhenNothingCouldBeMerged(t *testing.T) {
	bm := newTestManager(t, nil)

	_, resolved, err := bm.ValidateRawBlueprint([]byte("name: [unterminated\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved != nil {
		t.Fatalf("expected nil resolved for a syntax error, got %+v", resolved)
	}

	_, resolved, err = bm.ValidateRawBlueprint([]byte("name: my-blueprint\ntemplate: does-not-exist\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved != nil {
		t.Fatalf("expected nil resolved for an unresolvable template, got %+v", resolved)
	}
}

// TestValidateRawBlueprintResolvedNilWhenInvalid checks that no resolved
// document is returned when the blueprint fails validation: a caller should
// fix the reported issues first rather than being handed a preview built
// from an invalid document.
func TestValidateRawBlueprintResolvedNilWhenInvalid(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
k8shelld:
  image: k8shelld-image:latest
`)

	issues, resolved, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) == 0 {
		t.Fatalf("expected at least one issue for missing 'image' field")
	}
	if resolved != nil {
		t.Fatalf("expected nil resolved for an invalid blueprint, got %+v", resolved)
	}
}

// TestValidateRawBlueprintResolvedMergesTemplate checks that the resolved
// document carries fields inherited from the referenced Template, not just
// the submitted "own" content.
func TestValidateRawBlueprintResolvedMergesTemplate(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": `
name: base
isTemplate: true
description: Base template
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

	yaml := []byte("name: custom\ntemplate: base\ndescription: Custom blueprint\n")

	issues, resolved, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected no issues, got %+v", issues)
	}
	m, ok := resolved.(map[string]interface{})
	if !ok {
		t.Fatalf("expected resolved to decode to a map, got %T: %+v", resolved, resolved)
	}
	if m["image"] != "base-image:latest" {
		t.Fatalf("expected resolved to inherit 'image' from the template, got %+v", m)
	}
	if m["name"] != "custom" {
		t.Fatalf("expected resolved to keep the submitted 'name', got %+v", m)
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
description: A test blueprint
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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
description: A test blueprint
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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
description: A test blueprint
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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
description: A test blueprint
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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
description: A test blueprint
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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

	issues, _, err := bm.ValidateRawBlueprint(yaml)
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

// TestValidateRawBlueprintCELErrorFieldBracketsMapKey checks that a CEL
// failure inside a user-keyed map (a storage entry) reports its Field with
// the same bracketed convention every other issue uses ("storages[shared]"),
// not yaml-cel's own plain-dot path format ("storages.shared") — see
// bracketMapKeys.
func TestValidateRawBlueprintCELErrorFieldBracketsMapKey(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: my-blueprint
description: A test blueprint
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
storages:
  shared:
    enabled: true
    type: shared
    path: /opt/shared
    claimSpec:
      resources:
        requests:
          storage: 50Gi
      storageClassName: !cel envx("WORKSPACE_STORAGE_CLASS")
`)

	issues, _, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected exactly one issue, got %+v", issues)
	}
	want := "storages[shared].claimSpec.storageClassName"
	if issues[0].Field != want {
		t.Fatalf("expected field %q, got %+v", want, issues[0])
	}
}

func TestValidateRawBlueprintWithTemplate(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": `
name: base
isTemplate: true
description: Base template
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
description: Child blueprint
`)

	issues, _, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected no issues inheriting from a valid template, got %+v", issues)
	}
}

// TestValidateRawBlueprintSecurityContextRules checks the k8shelld-specific
// securityContext rules: runAsUser/runAsGroup must be 0, runAsNonRoot/
// readOnlyRootFilesystem cannot be true, allowPrivilegeEscalation cannot be
// false, and CHOWN/SETUID/SETGID (requiredCaps) must not be dropped, whether
// explicitly or via "drop: [ALL]" without re-adding them. It also covers the
// regression where a typo'd field (top-level or nested) was silently
// ignored by a non-strict JSON decode instead of being reported, which left
// spec.Capabilities nil and skipped every check below it.
func TestValidateRawBlueprintSecurityContextRules(t *testing.T) {
	bm := newTestManager(t, nil)
	base := "name: sec-test\ndescription: A test blueprint\nimage: myimage:latest\n" + requiredBlueprintFields

	cases := []struct {
		name      string
		secYAML   string
		wantIssue bool
		wantField string // substring; ignored when wantIssue is false
	}{
		{
			name:      "runAsUser non-zero",
			secYAML:   "securityContext:\n  runAsUser: 1000\n",
			wantIssue: true,
			wantField: "runAsUser",
		},
		{
			name:      "runAsGroup non-zero",
			secYAML:   "securityContext:\n  runAsGroup: 1000\n",
			wantIssue: true,
			wantField: "runAsGroup",
		},
		{
			name:      "runAsNonRoot true",
			secYAML:   "securityContext:\n  runAsNonRoot: true\n",
			wantIssue: true,
			wantField: "runAsNonRoot",
		},
		{
			name:      "readOnlyRootFilesystem true",
			secYAML:   "securityContext:\n  readOnlyRootFilesystem: true\n",
			wantIssue: true,
			wantField: "readOnlyRootFilesystem",
		},
		{
			name:      "allowPrivilegeEscalation false",
			secYAML:   "securityContext:\n  allowPrivilegeEscalation: false\n",
			wantIssue: true,
			wantField: "allowPrivilegeEscalation",
		},
		{
			name:      "drop ALL without re-adding required caps",
			secYAML:   "securityContext:\n  capabilities:\n    drop:\n      - ALL\n",
			wantIssue: true,
			wantField: "securityContext.capabilities",
		},
		{
			name:      "drop ALL with required caps re-added is fine",
			secYAML:   "securityContext:\n  capabilities:\n    drop:\n      - ALL\n    add:\n      - CHOWN\n      - SETUID\n      - SETGID\n",
			wantIssue: false,
		},
		{
			name:      "explicit drop of a required capability",
			secYAML:   "securityContext:\n  capabilities:\n    drop:\n      - CHOWN\n",
			wantIssue: true,
			wantField: "securityContext.capabilities",
		},
		{
			name:      "unknown top-level field is rejected, not silently ignored",
			secYAML:   "securityContext:\n  capabilitiesx:\n    add:\n      - SYS_PTRACEx\n",
			wantIssue: true,
			wantField: "securityContext",
		},
		{
			name:      "unknown nested field is rejected",
			secYAML:   "securityContext:\n  capabilities:\n    addx:\n      - SYS_PTRACE\n",
			wantIssue: true,
			wantField: "securityContext",
		},
		{
			name:      "valid securityContext with an extra added capability",
			secYAML:   "securityContext:\n  runAsUser: 0\n  runAsGroup: 0\n  allowPrivilegeEscalation: true\n  capabilities:\n    add:\n      - SYS_PTRACE\n",
			wantIssue: false,
		},
		{
			name:      "bogus capability name in add is rejected",
			secYAML:   "securityContext:\n  capabilities:\n    add:\n      - SYS_PTRACEx\n",
			wantIssue: true,
			wantField: "securityContext.capabilities.add",
		},
		{
			name:      "bogus capability name in drop is rejected",
			secYAML:   "securityContext:\n  capabilities:\n    drop:\n      - NOT_A_REAL_CAP\n",
			wantIssue: true,
			wantField: "securityContext.capabilities.drop",
		},
		{
			name:      "ALL is accepted as a drop wildcard, not a bogus capability",
			secYAML:   "securityContext:\n  capabilities:\n    drop:\n      - ALL\n    add:\n      - CHOWN\n      - SETUID\n      - SETGID\n",
			wantIssue: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues, _, err := bm.ValidateRawBlueprint([]byte(base + tc.secYAML))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			found := false
			for _, issue := range issues {
				if strings.Contains(issue.Field, tc.wantField) {
					found = true
				}
			}
			if tc.wantIssue && !found {
				t.Fatalf("expected an issue with field containing %q, got %+v", tc.wantField, issues)
			}
			if !tc.wantIssue && len(issues) != 0 {
				t.Fatalf("expected no issues, got %+v", issues)
			}
		})
	}
}

// TestValidateRawBlueprintPodmanCapabilityNames checks that a bogus
// capability name under podman.securityContext.capabilities is rejected the
// same way it is for the workspace securityContext.
func TestValidateRawBlueprintPodmanCapabilityNames(t *testing.T) {
	bm := newTestManager(t, nil)

	yaml := []byte(`
name: sec-test
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
  securityContext:
    capabilities:
      add:
        - SYS_PTRACEx
`)

	issues, _, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, issue := range issues {
		if strings.Contains(issue.Field, "podman.securityContext.capabilities.add") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an issue for the bogus podman capability, got %+v", issues)
	}
}

// TestValidateRawBlueprintStorageOwnerIDs checks fsOwnerUid/fsOwnerGid
// validation on storage entries. FsOwnerUid/FsOwnerGid are *int fields on
// models.Storage, so a non-numeric value fails yaml.v3's struct decode with
// a generic, unpathed TypeError and (per yaml.v3's own behavior) leaves the
// pointer non-nil pointing at zero — indistinguishable from a legitimately
// set "fsOwnerUid: 0". validateStorageOwnerIDs checks the raw, still-intact
// value instead, so it must report a properly field-pathed issue.
func TestValidateRawBlueprintStorageOwnerIDs(t *testing.T) {
	bm := newTestManager(t, nil)
	base := "name: sec-test\ndescription: A test blueprint\nimage: myimage:latest\n" +
		"k8shelld:\n  image: k8shelld-image:latest\nresources:\n  cpu: 500m\n  memory: 512Mi\n"

	cases := []struct {
		name      string
		extra     string
		wantIssue bool
		wantField string
	}{
		{
			name: "non-numeric fsOwnerUid on a workspace storage",
			extra: "podman:\n  resources:\n    cpu: 500m\n    memory: 512Mi\n" +
				"storages:\n  home:\n    path: /home/x\n    fsOwnerUid: abc\n",
			wantIssue: true,
			wantField: "storages[home].fsOwnerUid",
		},
		{
			name: "non-numeric fsOwnerGid on a podman storage",
			extra: "podman:\n  resources:\n    cpu: 500m\n    memory: 512Mi\n" +
				"  storages:\n    data:\n      path: /data\n      fsOwnerGid: abc\n",
			wantIssue: true,
			wantField: "podman.storages[data].fsOwnerGid",
		},
		{
			name: "valid numeric fsOwnerUid/fsOwnerGid",
			extra: "podman:\n  resources:\n    cpu: 500m\n    memory: 512Mi\n" +
				"storages:\n  home:\n    path: /home/x\n    fsOwnerUid: 1001\n    fsOwnerGid: 1001\n",
			wantIssue: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues, resolved, err := bm.ValidateRawBlueprint([]byte(base + tc.extra))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			found := false
			for _, issue := range issues {
				if issue.Field == tc.wantField {
					found = true
				}
			}
			if tc.wantIssue {
				if !found {
					t.Fatalf("expected an issue with field %q, got %+v", tc.wantField, issues)
				}
				if resolved != nil {
					t.Fatalf("expected nil resolved for an invalid submission, got %+v", resolved)
				}
			} else if len(issues) != 0 {
				t.Fatalf("expected no issues, got %+v", issues)
			}
		})
	}
}

// TestValidateRawBlueprintClaimSpecResourceNames checks that
// claimSpec.resources.requests/limits reject any resource name other than
// "storage" — the only one Kubernetes accepts for a PersistentVolumeClaim.
// Requests/Limits are plain map[ResourceName]Quantity, so a typo'd key
// (e.g. "storagex") decodes without error from DisallowUnknownFields, which
// only rejects unrecognized struct fields, never map keys.
func TestValidateRawBlueprintClaimSpecResourceNames(t *testing.T) {
	bm := newTestManager(t, nil)
	base := "name: sec-test\ndescription: A test blueprint\nimage: myimage:latest\n" + requiredBlueprintFields

	cases := []struct {
		name      string
		extra     string
		wantIssue bool
		wantField string
	}{
		{
			name: "typo'd resource name in requests",
			extra: "storages:\n  shared:\n    path: /opt/shared\n    claimSpec:\n" +
				"      resources:\n        requests:\n          storage: 50Gi\n          storagex: 50Gi\n",
			wantIssue: true,
			wantField: "storages[shared].claimSpec.resources.requests",
		},
		{
			name: "typo'd resource name in limits",
			extra: "storages:\n  shared:\n    path: /opt/shared\n    claimSpec:\n" +
				"      resources:\n        limits:\n          storagex: 50Gi\n",
			wantIssue: true,
			wantField: "storages[shared].claimSpec.resources.limits",
		},
		{
			name: "valid storage-only requests",
			extra: "storages:\n  shared:\n    path: /opt/shared\n    claimSpec:\n" +
				"      resources:\n        requests:\n          storage: 50Gi\n",
			wantIssue: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues, resolved, err := bm.ValidateRawBlueprint([]byte(base + tc.extra))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			found := false
			for _, issue := range issues {
				if issue.Field == tc.wantField {
					found = true
				}
			}
			if tc.wantIssue {
				if !found {
					t.Fatalf("expected an issue with field %q, got %+v", tc.wantField, issues)
				}
				if resolved != nil {
					t.Fatalf("expected nil resolved for an invalid submission, got %+v", resolved)
				}
			} else if len(issues) != 0 {
				t.Fatalf("expected no issues, got %+v", issues)
			}
		})
	}
}
