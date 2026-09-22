// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"errors"
	"testing"
	"time"

	queryv1 "github.com/k8shell-io/common/pkg/api/gen/go/query/v1"
	"github.com/k8shell-io/common/pkg/models"
)

func newQueryTestManager(t *testing.T) *BlueprintManager {
	t.Helper()

	store := &fakeOrgStore{}
	bm := newTestManagerWithOrgStore(t, map[string]string{
		"alpha.yaml": "name: alpha\ndescription: file blueprint\nimage: myimage:latest\n" + requiredBlueprintFields,
		"beta.yaml":  "name: beta\ndescription: file blueprint\nimage: myimage:latest\n" + requiredBlueprintFields,
	}, store)

	store.set([]*models.OrgBlueprint{
		{
			Org:       "acme",
			Name:      "gamma",
			YAML:      []byte("name: gamma\ndescription: org blueprint\nimage: acme-image:latest\n" + requiredBlueprintFields),
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	if err := bm.ReloadOrgBlueprints(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	return bm
}

func names(summaries []*models.BlueprintSummary) []string {
	out := make([]string, len(summaries))
	for i, s := range summaries {
		out[i] = s.Name
	}
	return out
}

func equalNames(t *testing.T, got []*models.BlueprintSummary, want []string) {
	t.Helper()
	gotNames := names(got)
	if len(gotNames) != len(want) {
		t.Fatalf("expected names %v, got %v", want, gotNames)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("expected names %v, got %v", want, gotNames)
		}
	}
}

// unrestrictedObligations grants visibility into every organization's
// blueprints (see blueprintObligationScope), so tests that exercise
// filter/sort/page mechanics unrelated to obligation scoping can see gamma
// (org "acme") alongside the file-based globals.
func unrestrictedObligations() map[string]string {
	return map[string]string{"org": obligationOrgWildcard}
}

func TestQueryBlueprints_DefaultSortIsByName(t *testing.T) {
	bm := newQueryTestManager(t)

	got, err := bm.QueryBlueprints(&queryv1.Payload{Obligations: unrestrictedObligations()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"alpha", "beta", "gamma"})
}

func TestQueryBlueprints_FilterByName(t *testing.T) {
	bm := newQueryTestManager(t)

	payload := &queryv1.Payload{
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "name", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"alpha"}},
			},
		},
	}
	got, err := bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"alpha"})
}

func TestQueryBlueprints_FilterByOrg(t *testing.T) {
	bm := newQueryTestManager(t)

	payload := &queryv1.Payload{
		Obligations: unrestrictedObligations(),
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "org", Op: queryv1.Operator_OPERATOR_NE, Values: []string{"acme"}},
			},
		},
	}
	got, err := bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"alpha", "beta"})

	payload = &queryv1.Payload{
		Obligations: unrestrictedObligations(),
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "org", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"acme"}},
			},
		},
	}
	got, err = bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"gamma"})
}

func TestQueryBlueprints_FilterByGlobal(t *testing.T) {
	bm := newQueryTestManager(t)

	payload := &queryv1.Payload{
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "global", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"true"}},
			},
		},
	}
	got, err := bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"alpha", "beta"})

	payload = &queryv1.Payload{
		Obligations: unrestrictedObligations(),
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "global", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"false"}},
			},
		},
	}
	got, err = bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"gamma"})

	payload = &queryv1.Payload{
		Obligations: unrestrictedObligations(),
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "global", Op: queryv1.Operator_OPERATOR_NE, Values: []string{"true"}},
			},
		},
	}
	got, err = bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"gamma"})
}

func TestQueryBlueprints_FilterByCreated(t *testing.T) {
	bm := newQueryTestManager(t)

	payload := &queryv1.Payload{
		Obligations: unrestrictedObligations(),
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "created", Op: queryv1.Operator_OPERATOR_LT, Values: []string{"2026-06-01"}},
			},
		},
	}
	got, err := bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"gamma"})
}

func TestQueryBlueprints_SortDescAndPage(t *testing.T) {
	bm := newQueryTestManager(t)

	payload := &queryv1.Payload{
		Obligations: unrestrictedObligations(),
		Sort:        []*queryv1.Sort{{Field: "name", Dir: queryv1.SortDir_SORT_DIR_DESC}},
		Page:        &queryv1.Page{Limit: 1, Offset: 1},
	}
	got, err := bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"beta"})
}

func TestQueryBlueprints_ObligationsMissingOrgScopesToGlobalsOnly(t *testing.T) {
	bm := newQueryTestManager(t)

	got, err := bm.QueryBlueprints(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"alpha", "beta"})
}

func TestQueryBlueprints_ObligationsScopeToOrgPlusGlobals(t *testing.T) {
	bm := newQueryTestManager(t)

	payload := &queryv1.Payload{Obligations: map[string]string{"org": "acme"}}
	got, err := bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"alpha", "beta", "gamma"})

	payload = &queryv1.Payload{Obligations: map[string]string{"org": "other-org"}}
	got, err = bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"alpha", "beta"})
}

func TestQueryBlueprints_ObligationsWildcardOrgIsUnrestricted(t *testing.T) {
	bm := newQueryTestManager(t)

	payload := &queryv1.Payload{Obligations: unrestrictedObligations()}
	got, err := bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"alpha", "beta", "gamma"})
}

func TestQueryBlueprints_ObligationsCombineWithFiltersAsAnd(t *testing.T) {
	bm := newQueryTestManager(t)

	// The obligation scopes visibility to acme's blueprints plus globals;
	// the client's own filter further narrows to org "acme" — gamma is the
	// only blueprint satisfying both.
	payload := &queryv1.Payload{
		Obligations: map[string]string{"org": "acme"},
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "org", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"acme"}},
			},
		},
	}
	got, err := bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{"gamma"})

	// A filter for a different org can never match once the obligation
	// scopes visibility to acme plus globals.
	payload = &queryv1.Payload{
		Obligations: map[string]string{"org": "acme"},
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "org", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"other-org"}},
			},
		},
	}
	got, err = bm.QueryBlueprints(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	equalNames(t, got, []string{})
}

func TestQueryBlueprints_UnknownFieldIsInvalid(t *testing.T) {
	bm := newQueryTestManager(t)

	payload := &queryv1.Payload{
		Filters: &queryv1.Filters{
			Conditions: []*queryv1.Condition{
				{Field: "nope", Op: queryv1.Operator_OPERATOR_EQ, Values: []string{"x"}},
			},
		},
	}
	_, err := bm.QueryBlueprints(payload)
	if !errors.Is(err, models.ErrInvalidParameters) {
		t.Fatalf("expected ErrInvalidParameters, got %v", err)
	}
}
