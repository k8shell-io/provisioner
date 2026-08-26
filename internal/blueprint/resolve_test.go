// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMergeYAMLNodesExcludesDescription checks, at the raw node-merge level,
// that a child which doesn't set its own "description" does not pick up the
// template's — the same treatment "isTemplate" already gets, and for the
// same reason: a child of a template is not itself a template unless it
// says so, and every blueprint must describe itself rather than borrow its
// template's description.
func TestMergeYAMLNodesExcludesDescriptionAndIsTemplate(t *testing.T) {
	bm := &BlueprintManager{}

	var parentDoc, childDoc yaml.Node
	if err := yaml.Unmarshal([]byte("name: base\nisTemplate: true\ndescription: \"Base description\"\nimage: base-image\n"), &parentDoc); err != nil {
		t.Fatalf("parent unmarshal: %v", err)
	}
	if err := yaml.Unmarshal([]byte("name: child\ntemplate: base\nimage: child-image\n"), &childDoc); err != nil {
		t.Fatalf("child unmarshal: %v", err)
	}

	merged, err := bm.mergeYAMLNodes(parentDoc.Content[0], childDoc.Content[0])
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}

	var out map[string]interface{}
	if err := merged.Decode(&out); err != nil {
		t.Fatalf("decode merged: %v", err)
	}

	if v, ok := out["description"]; ok {
		t.Fatalf("expected description to not be inherited from the template, got %+v", v)
	}
	if v, ok := out["isTemplate"]; ok {
		t.Fatalf("expected isTemplate to not be inherited from the template, got %+v", v)
	}
	if out["image"] != "child-image" {
		t.Fatalf("expected the child's own image to survive the merge, got %+v", out["image"])
	}
}

// TestDescriptionOverridesTemplate checks that a blueprint's own description
// is what's used — both in the summary view and the fully evaluated
// blueprint — never the template's, matching
// TestMergeYAMLNodesExcludesDescriptionAndIsTemplate above.
func TestDescriptionOverridesTemplate(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": "name: base\nisTemplate: true\ndescription: \"Base template description\"\nimage: base-image:latest\n" +
			requiredBlueprintFields,
		"child.yaml": "name: child\ntemplate: base\ndescription: \"Child's own description\"\nimage: child-image:latest\n" +
			requiredBlueprintFields,
	})

	for _, s := range bm.GetBlueprintsSummary() {
		if s.Name == "child" && s.Description != "Child's own description" {
			t.Fatalf("expected summary description to keep the child's own, got %q", s.Description)
		}
	}

	bp, err := bm.GetBlueprint("child", TestScope())
	if err != nil {
		t.Fatalf("GetBlueprint failed: %v", err)
	}
	if bp.Description != "Child's own description" {
		t.Fatalf("expected evaluated description to keep the child's own, got %q", bp.Description)
	}
}

// TestValidateRawBlueprintDescriptionRequired checks that submitting a
// blueprint without its own description is rejected, even when it inherits
// from a template that has one — description is required on every blueprint
// (validateDescriptionRequired, gated by the RequireDescription switch), not
// something a child can pick up from its template.
func TestValidateRawBlueprintDescriptionRequired(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": "name: base\nisTemplate: true\ndescription: \"Base template description\"\nimage: base-image:latest\n" +
			requiredBlueprintFields,
	})

	yaml := []byte("name: child\ntemplate: base\nimage: child-image:latest\n" + requiredBlueprintFields)

	t.Cleanup(func() { RequireDescription = false })
	RequireDescription = true

	issues, resolved, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, issue := range issues {
		if strings.Contains(strings.ToLower(issue.Field), "description") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 'description' issue for a submission without one, got %+v", issues)
	}
	if resolved != nil {
		t.Fatalf("expected nil resolved for an invalid submission, got %+v", resolved)
	}
}

// TestRequireDescriptionSwitch checks that flipping RequireDescription to
// false allows a blueprint with no description through, and that flipping
// it back on rejects it again — the toggle takes effect immediately since
// validateDescriptionRequired reads it on every call rather than caching it.
func TestRequireDescriptionSwitch(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": "name: base\nisTemplate: true\ndescription: \"Base template description\"\nimage: base-image:latest\n" +
			requiredBlueprintFields,
	})

	yaml := []byte("name: child\ntemplate: base\nimage: child-image:latest\n" + requiredBlueprintFields)

	t.Cleanup(func() { RequireDescription = false })

	RequireDescription = false
	issues, resolved, err := bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected no issues with RequireDescription=false, got %+v", issues)
	}
	if resolved == nil {
		t.Fatalf("expected a resolved document for a valid submission")
	}

	RequireDescription = true
	issues, _, err = bm.ValidateRawBlueprint(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(strings.ToLower(issue.Field), "description") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 'description' issue once RequireDescription is true again, got %+v", issues)
	}
}
