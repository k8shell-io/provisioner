// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/identity"
	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	queryv1 "github.com/k8shell-io/common/pkg/api/gen/go/query/v1"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/query"
	"github.com/k8shell-io/provisioner/internal/helm"
)

// WorkspacesQueryDescriptor advertises which workspace fields are
// queryable/sortable via QueryWorkspaces, and which operators are valid on
// each. It is served as-is by GetWorkspacesQuerySchema and reused by
// QueryWorkspaces to validate incoming payloads, so the advertised schema
// and its enforcement can never drift apart.
var WorkspacesQueryDescriptor = query.NewDescriptor("workspaces").
	Field("name", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN).
	Field("username", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN).
	Field("organization", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN).
	Field("blueprint", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN).
	Field("namespace", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN).
	Field("status", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN).
	Field("repo_owner", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN, queryv1.Operator_OPERATOR_EXISTS).
	Field("repo_name", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN, queryv1.Operator_OPERATOR_EXISTS).
	Field("repo_ref", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN, queryv1.Operator_OPERATOR_EXISTS).
	Field("created", queryv1.FieldType_FIELD_TYPE_DATETIME,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE,
		queryv1.Operator_OPERATOR_GT, queryv1.Operator_OPERATOR_GTE,
		queryv1.Operator_OPERATOR_LT, queryv1.Operator_OPERATOR_LTE).
	DefaultSort("name", queryv1.SortDir_SORT_DIR_ASC).
	Build()

// workspaceFieldValues extracts, for each field declared in
// WorkspacesQueryDescriptor, its comparable string value from a
// WorkspaceDetails record and whether the field is actually set.
var workspaceFieldValues = map[string]func(w *models.WorkspaceDetails) (string, bool){
	"name":         func(w *models.WorkspaceDetails) (string, bool) { return w.Name, w.Name != "" },
	"username":     func(w *models.WorkspaceDetails) (string, bool) { return w.Username, w.Username != "" },
	"organization": func(w *models.WorkspaceDetails) (string, bool) { return w.Organization, w.Organization != "" },
	"blueprint":    func(w *models.WorkspaceDetails) (string, bool) { return w.Blueprint, w.Blueprint != "" },
	"namespace":    func(w *models.WorkspaceDetails) (string, bool) { return w.Namespace, w.Namespace != "" },
	"status":       func(w *models.WorkspaceDetails) (string, bool) { return string(w.Status), w.Status != "" },
	"repo_owner":   func(w *models.WorkspaceDetails) (string, bool) { return w.RepoOwner, w.RepoOwner != "" },
	"repo_name":    func(w *models.WorkspaceDetails) (string, bool) { return w.RepoName, w.RepoName != "" },
	"repo_ref":     func(w *models.WorkspaceDetails) (string, bool) { return w.RepoRef, w.RepoRef != "" },
	"created": func(w *models.WorkspaceDetails) (string, bool) {
		if w.Created.IsZero() {
			return "", false
		}
		return w.Created.UTC().Format(time.RFC3339), true
	},
}

// obligationScope is the mandatory, authz-derived restriction the
// workspace:list obligations (see common/pkg/authz/workspace.go) place on a
// QueryWorkspaces result, ANDed on independent of the client's own Filters.
// org and username narrow workspace fields directly; roles has no
// workspace-side equivalent, so resolveObligationScope resolves it to the
// set of usernames holding one of those roles via identity.
type obligationScope struct {
	organization string   // "" = unrestricted
	usernames    []string // nil = unrestricted; non-nil (possibly empty) restricts to these
}

// matches reports whether w satisfies the scope. It is the authoritative
// enforcement of obligations — narrow only optimizes what gets fetched.
func (s obligationScope) matches(w *models.WorkspaceDetails) bool {
	if s.organization != "" && !strings.EqualFold(w.Organization, s.organization) {
		return false
	}
	if s.usernames != nil && !slices.ContainsFunc(s.usernames, func(u string) bool {
		return strings.EqualFold(u, w.Username)
	}) {
		return false
	}
	return true
}

// narrow folds the scope into opts as an additional fetch-side restriction,
// on top of whatever pushdownOptionsFromPayload already derived from the
// client's Filters. Like that pushdown, this is purely an optimization —
// matches is what actually enforces the scope, so an obligation is never
// weakened even if narrow leaves it out of the Kubernetes selector.
func (s obligationScope) narrow(opts *GetWorkspacesOptions) {
	if s.organization != "" {
		opts.Organization = s.organization
	}
	if s.usernames != nil {
		opts.Usernames = intersectUsernames(opts.Usernames, s.usernames)
	}
}

// resolveObligationScope turns payload.Obligations — populated by an
// upstream gateway after evaluating the workspace:list policy, never
// client-supplied — into the obligationScope QueryWorkspaces must enforce.
// A roles obligation requires identityClient to resolve which usernames
// hold one of the given roles; it is an error to present one without an
// identityClient, since silently ignoring it would drop an authz
// restriction rather than just fetch less efficiently.
func resolveObligationScope(ctx context.Context, identityClient *identity.IdentityClient,
	obligations map[string]string) (obligationScope, error) {
	var scope obligationScope

	if org, ok := authz.ParseOrgObligation(obligations); ok {
		scope.organization = org.Org
	}
	if user, ok := authz.ParseUsernameObligation(obligations); ok {
		scope.usernames = []string{user.Username}
	}

	roles, ok := authz.ParseRolesObligation(obligations)
	if !ok || len(roles.Roles) == 0 {
		return scope, nil
	}
	if identityClient == nil {
		return obligationScope{}, fmt.Errorf("roles obligation present but no identity client configured")
	}

	roleValues := make([]string, 0, len(roles.Roles))
	for _, r := range roles.Roles {
		roleValues = append(roleValues, string(r))
	}
	resp, err := identityClient.GetUsers(ctx, &identityv1.GetUsersRequest{Roles: roleValues})
	if err != nil {
		return obligationScope{}, fmt.Errorf("failed to resolve roles obligation: %w", err)
	}
	roleUsernames := make([]string, 0, len(resp.GetUsers()))
	for _, u := range resp.GetUsers() {
		roleUsernames = append(roleUsernames, u.GetUsername())
	}
	scope.usernames = intersectUsernames(scope.usernames, roleUsernames)

	return scope, nil
}

// intersectUsernames returns the case-insensitive intersection of a and b.
// A nil slice means "unrestricted": intersecting with nil returns the other
// operand unchanged, and intersecting two non-nil slices always returns a
// non-nil (possibly empty) result.
func intersectUsernames(a, b []string) []string {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if slices.ContainsFunc(b, func(u string) bool { return strings.EqualFold(u, v) }) {
			out = append(out, v)
		}
	}
	return out
}

// QueryWorkspaces returns workspaces matching payload, as validated against
// WorkspacesQueryDescriptor and, independent of payload's own Filters,
// restricted to whatever payload.Obligations mandates (see obligationScope).
// Workspace state lives in Kubernetes rather than a queryable store, so
// unlike a SQL-backed resource this can't push an arbitrary filter down as a
// single query: it discovers workspaces the same way GetWorkspaces does
// (standalone plus, when injectNamespaces is set, injected workspaces) and
// applies payload's filters/sort/page in memory.
//
// Listing every workspace on every call would still be wasteful against
// etcd/apiserver, so equality/IN conditions on username, organization,
// blueprint, or name — from either Filters or Obligations — are pushed down
// into GetWorkspacesOptions first, which GetWorkspaces already compiles into
// a Kubernetes label selector — or, for an exact name match, a direct Get
// instead of a List. That's a best-effort fetch-side optimization only: the
// in-memory checks below still re-verify the full payload and obligations
// against whatever comes back, so correctness never depends on the pushdown
// being complete.
//
// identityClient resolves the obligations' "roles" dimension, which has no
// workspace-side equivalent (workspaces carry no role information); it may
// be nil as long as no caller ever presents a roles obligation.
func QueryWorkspaces(ctx context.Context, helmClient *helm.Client, identityClient *identity.IdentityClient,
	injectNamespaces []string, payload *queryv1.Payload) (*GetWorkspacesResult, error) {
	if err := query.Validate(WorkspacesQueryDescriptor, payload); err != nil {
		return nil, fmt.Errorf("%w: %s", models.ErrInvalidParameters, err)
	}

	scope, err := resolveObligationScope(ctx, identityClient, payload.GetObligations())
	if err != nil {
		return nil, fmt.Errorf("failed to resolve query obligations: %w", err)
	}
	if scope.usernames != nil && len(scope.usernames) == 0 {
		// The obligations restrict results to a set of usernames that
		// resolved empty (e.g. no user holds any of the required roles) —
		// nothing can match, so skip the Kubernetes listing entirely.
		return &GetWorkspacesResult{}, nil
	}

	opts := pushdownOptionsFromPayload(payload)
	scope.narrow(&opts)
	opts.InjectNamespaces = injectNamespaces

	result, err := GetWorkspaces(ctx, helmClient, opts)
	if err != nil {
		return nil, err
	}

	filtered := make([]*models.WorkspaceDetails, 0, len(result.Workspaces))
	for _, w := range result.Workspaces {
		if !scope.matches(w) {
			continue
		}
		matched, err := matchesFilters(w, payload.GetFilters())
		if err != nil {
			return nil, fmt.Errorf("%w: %s", models.ErrInvalidParameters, err)
		}
		if matched {
			filtered = append(filtered, w)
		}
	}

	sorts := payload.GetSort()
	if len(sorts) == 0 {
		sorts = WorkspacesQueryDescriptor.GetDefaultSort()
	}
	sortWorkspaces(filtered, sorts)

	return &GetWorkspacesResult{Workspaces: pageWorkspaces(filtered, payload.GetPage())}, nil
}

// pushdownOptionsFromPayload extracts equality/IN conditions on username,
// organization, blueprint, or name into a GetWorkspacesOptions, so the
// caller can fetch a narrower set from Kubernetes instead of listing every
// workspace. Only AND-combined conditions are eligible — an OR group can't
// be expressed as a single label selector without risking a false
// exclusion, so it's left entirely to the in-memory filter. A field already
// populated by an earlier condition is left alone rather than overwritten;
// any condition that isn't pushed down is still enforced in memory, so
// skipping one here only costs fetch-side narrowing, never correctness.
func pushdownOptionsFromPayload(payload *queryv1.Payload) GetWorkspacesOptions {
	var opts GetWorkspacesOptions

	filters := payload.GetFilters()
	if filters == nil || filters.GetOp() == queryv1.FilterOp_FILTER_OP_OR {
		return opts
	}

	for _, c := range filters.GetConditions() {
		switch c.GetField() {
		case "username":
			if opts.Usernames != nil {
				continue
			}
			opts.Usernames = pushableValues(c.GetOp(), c.GetValues())
		case "organization":
			if opts.Organization != "" {
				continue
			}
			if v := pushableValues(c.GetOp(), c.GetValues()); len(v) == 1 {
				opts.Organization = v[0]
			}
		case "blueprint":
			if opts.Blueprint != "" {
				continue
			}
			if v := pushableValues(c.GetOp(), c.GetValues()); len(v) == 1 {
				opts.Blueprint = v[0]
			}
		case "name":
			if opts.WorkspaceName != "" {
				continue
			}
			if v := pushableValues(c.GetOp(), c.GetValues()); len(v) == 1 {
				opts.WorkspaceName = v[0]
			}
		}
	}

	return opts
}

// pushableValues returns values unchanged when they can be expressed
// exactly as a Kubernetes label-selector equality/"in" match: op is EQ or
// IN, and no value carries a glob wildcard (the selector has no wildcard
// support). Returns nil otherwise.
func pushableValues(op queryv1.Operator, values []string) []string {
	if op != queryv1.Operator_OPERATOR_EQ && op != queryv1.Operator_OPERATOR_IN {
		return nil
	}
	for _, v := range values {
		if strings.ContainsAny(v, "*?") {
			return nil
		}
	}
	return values
}

func fieldType(name string) queryv1.FieldType {
	for _, f := range WorkspacesQueryDescriptor.GetFields() {
		if f.GetName() == name {
			return f.GetType()
		}
	}
	return queryv1.FieldType_FIELD_TYPE_UNSPECIFIED
}

func matchesFilters(w *models.WorkspaceDetails, filters *queryv1.Filters) (bool, error) {
	conditions := filters.GetConditions()
	if len(conditions) == 0 {
		return true, nil
	}

	or := filters.GetOp() == queryv1.FilterOp_FILTER_OP_OR
	for _, c := range conditions {
		matched, err := matchesCondition(w, c)
		if err != nil {
			return false, err
		}
		if or && matched {
			return true, nil
		}
		if !or && !matched {
			return false, nil
		}
	}
	return !or, nil
}

func matchesCondition(w *models.WorkspaceDetails, c *queryv1.Condition) (bool, error) {
	extract, ok := workspaceFieldValues[c.GetField()]
	if !ok {
		return false, fmt.Errorf("query: unknown field %q", c.GetField())
	}
	typ := fieldType(c.GetField())
	value, present := extract(w)

	switch c.GetOp() {
	case queryv1.Operator_OPERATOR_EXISTS:
		want := true
		if len(c.GetValues()) == 1 {
			want = c.GetValues()[0] == "true"
		}
		return present == want, nil

	case queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE:
		matched := false
		if present {
			var err error
			if typ == queryv1.FieldType_FIELD_TYPE_DATETIME {
				matched, err = matchesDatetime(value, queryv1.Operator_OPERATOR_EQ, c.GetValues()[0])
			} else {
				matched = globMatch(c.GetValues()[0], value)
			}
			if err != nil {
				return false, err
			}
		}
		if c.GetOp() == queryv1.Operator_OPERATOR_NE {
			return !matched, nil
		}
		return matched, nil

	case queryv1.Operator_OPERATOR_IN:
		if !present {
			return false, nil
		}
		for _, v := range c.GetValues() {
			if strings.EqualFold(v, value) {
				return true, nil
			}
		}
		return false, nil

	case queryv1.Operator_OPERATOR_GT, queryv1.Operator_OPERATOR_GTE,
		queryv1.Operator_OPERATOR_LT, queryv1.Operator_OPERATOR_LTE:
		if !present {
			return false, nil
		}
		return matchesDatetime(value, c.GetOp(), c.GetValues()[0])

	default:
		return false, fmt.Errorf("query: unsupported operator %s", c.GetOp())
	}
}

func matchesDatetime(fieldValue string, op queryv1.Operator, raw string) (bool, error) {
	fieldTime, err := time.Parse(time.RFC3339, fieldValue)
	if err != nil {
		return false, fmt.Errorf("invalid stored datetime %q: %w", fieldValue, err)
	}
	parsed, err := query.ParseValue(queryv1.FieldType_FIELD_TYPE_DATETIME, raw)
	if err != nil {
		return false, err
	}
	condTime := parsed.(time.Time)

	switch op {
	case queryv1.Operator_OPERATOR_EQ:
		return fieldTime.Equal(condTime), nil
	case queryv1.Operator_OPERATOR_GT:
		return fieldTime.After(condTime), nil
	case queryv1.Operator_OPERATOR_GTE:
		return fieldTime.After(condTime) || fieldTime.Equal(condTime), nil
	case queryv1.Operator_OPERATOR_LT:
		return fieldTime.Before(condTime), nil
	case queryv1.Operator_OPERATOR_LTE:
		return fieldTime.Before(condTime) || fieldTime.Equal(condTime), nil
	default:
		return false, fmt.Errorf("query: unsupported datetime operator %s", op)
	}
}

// globMatch reports whether value case-insensitively matches pattern, using
// the same "*" (any run of characters) / "?" (any single character)
// wildcard vocabulary as query.v1.Operator's EQ/NE glob support.
func globMatch(pattern, value string) bool {
	return globMatchLower(strings.ToLower(pattern), strings.ToLower(value))
}

func globMatchLower(pattern, value string) bool {
	if pattern == "" {
		return value == ""
	}
	switch pattern[0] {
	case '*':
		if len(pattern) == 1 {
			return true
		}
		for i := 0; i <= len(value); i++ {
			if globMatchLower(pattern[1:], value[i:]) {
				return true
			}
		}
		return false
	case '?':
		if len(value) == 0 {
			return false
		}
		return globMatchLower(pattern[1:], value[1:])
	default:
		if len(value) == 0 || value[0] != pattern[0] {
			return false
		}
		return globMatchLower(pattern[1:], value[1:])
	}
}

func sortWorkspaces(list []*models.WorkspaceDetails, sorts []*queryv1.Sort) {
	if len(sorts) == 0 {
		return
	}
	sort.SliceStable(list, func(i, j int) bool {
		for _, s := range sorts {
			extract, ok := workspaceFieldValues[s.GetField()]
			if !ok {
				continue
			}
			vi, _ := extract(list[i])
			vj, _ := extract(list[j])
			if vi == vj {
				continue
			}
			if s.GetDir() == queryv1.SortDir_SORT_DIR_DESC {
				return vi > vj
			}
			return vi < vj
		}
		return false
	})
}

func pageWorkspaces(list []*models.WorkspaceDetails, page *queryv1.Page) []*models.WorkspaceDetails {
	if page == nil {
		return list
	}
	offset := int(page.GetOffset())
	if offset < 0 {
		offset = 0
	}
	if offset > len(list) {
		return []*models.WorkspaceDetails{}
	}
	end := len(list)
	if limit := int(page.GetLimit()); limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return list[offset:end]
}
