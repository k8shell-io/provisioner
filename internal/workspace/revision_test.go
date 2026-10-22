// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"testing"

	queryv1 "github.com/k8shell-io/common/pkg/api/gen/go/query/v1"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/query"
)

func TestMarkBlueprintOutdated(t *testing.T) {
	current := map[[2]string]string{
		{"acme", "dev"}:  "rev-new",
		{"other", "dev"}: "rev-old",
	}
	calls := 0
	revisionOf := func(org, name string) string {
		calls++
		return current[[2]string{org, name}]
	}

	ws := map[string]*models.WorkspaceDetails{
		"outdated":   {Organization: "acme", Blueprint: "dev", BlueprintRevision: "rev-old"},
		"current":    {Organization: "acme", Blueprint: "dev", BlueprintRevision: "rev-new"},
		"other-org":  {Organization: "other", Blueprint: "dev", BlueprintRevision: "rev-old"},
		"no-rev":     {Organization: "acme", Blueprint: "dev"},
		"gone":       {Organization: "acme", Blueprint: "deleted", BlueprintRevision: "rev-old"},
		"repo-based": {Organization: "acme", Blueprint: "dev", BlueprintRevision: "rev-old", RepoName: "app"},
	}
	list := make([]*models.WorkspaceDetails, 0, len(ws))
	for _, w := range ws {
		list = append(list, w)
	}
	list = append(list, nil)

	MarkBlueprintOutdated(list, revisionOf)

	want := map[string]*bool{
		"outdated":   ptr(true),
		"current":    ptr(false),
		"other-org":  ptr(false),
		"no-rev":     nil,
		"gone":       nil,
		"repo-based": nil,
	}
	for name, w := range ws {
		got, exp := w.BlueprintOutdated, want[name]
		switch {
		case exp == nil && got != nil:
			t.Errorf("%s: expected unknown, got %v", name, *got)
		case exp != nil && got == nil:
			t.Errorf("%s: expected %v, got unknown", name, *exp)
		case exp != nil && *got != *exp:
			t.Errorf("%s: expected %v, got %v", name, *exp, *got)
		}
	}
	// acme/dev, other/dev and acme/deleted, each resolved once.
	if calls != 3 {
		t.Errorf("expected 3 revision lookups, got %d", calls)
	}

	// A nil resolver leaves everything unknown.
	w := &models.WorkspaceDetails{Organization: "acme", Blueprint: "dev", BlueprintRevision: "rev-old"}
	MarkBlueprintOutdated([]*models.WorkspaceDetails{w}, nil)
	if w.BlueprintOutdated != nil {
		t.Errorf("nil resolver: expected unknown, got %v", *w.BlueprintOutdated)
	}
}

func TestQueryFiltersOnBlueprintRevisionFields(t *testing.T) {
	outdated := &models.WorkspaceDetails{Name: "a", BlueprintRevision: "r1", BlueprintOutdated: ptr(true)}
	current := &models.WorkspaceDetails{Name: "b", BlueprintRevision: "r2", BlueprintOutdated: ptr(false)}
	unknown := &models.WorkspaceDetails{Name: "c"}
	all := []*models.WorkspaceDetails{outdated, current, unknown}

	cases := []struct {
		name string
		cond *queryv1.Condition
		want []string
	}{
		{"outdated eq true", &queryv1.Condition{Field: "blueprint_outdated", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"true"}}, []string{"a"}},
		{"outdated eq 0", &queryv1.Condition{Field: "blueprint_outdated", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"0"}}, []string{"b"}},
		{"outdated ne true", &queryv1.Condition{Field: "blueprint_outdated", Op: queryv1.Operator_OPERATOR_NE, Values: []string{"true"}}, []string{"b", "c"}},
		{"outdated not exists", &queryv1.Condition{Field: "blueprint_outdated", Op: queryv1.Operator_OPERATOR_EXISTS, Values: []string{"false"}}, []string{"c"}},
		{"revision eq", &queryv1.Condition{Field: "blueprint_revision", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"r2"}}, []string{"b"}},
		{"revision in", &queryv1.Condition{Field: "blueprint_revision", Op: queryv1.Operator_OPERATOR_IN, Values: []string{"r1", "r2"}}, []string{"a", "b"}},
		{"revision exists", &queryv1.Condition{Field: "blueprint_revision", Op: queryv1.Operator_OPERATOR_EXISTS}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filters := &queryv1.Filters{Conditions: []*queryv1.Condition{tc.cond}}
			if err := query.Validate(WorkspacesQueryDescriptor, &queryv1.Payload{Filters: filters}); err != nil {
				t.Fatalf("descriptor rejected condition: %v", err)
			}
			var got []string
			for _, w := range all {
				ok, err := matchesFilters(w, filters)
				if err != nil {
					t.Fatalf("matchesFilters: %v", err)
				}
				if ok {
					got = append(got, w.Name)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("expected %v, got %v", tc.want, got)
				}
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
