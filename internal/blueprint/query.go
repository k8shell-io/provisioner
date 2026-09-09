// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	queryv1 "github.com/k8shell-io/common/pkg/api/gen/go/query/v1"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/query"
)

// BlueprintsQueryDescriptor advertises which blueprint fields are
// queryable/sortable via QueryBlueprints, and which operators are valid on
// each. It is served as-is by a GetBlueprintsQuerySchema RPC and reused by
// QueryBlueprints to validate incoming payloads, so the advertised schema and
// its enforcement can never drift apart.
var BlueprintsQueryDescriptor = query.NewDescriptor("blueprints").
	Field("name", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN).
	// org is empty for a file-based, global blueprint and non-empty for an
	// org-scoped database blueprint.
	Field("org", queryv1.FieldType_FIELD_TYPE_STRING,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE, queryv1.Operator_OPERATOR_IN).
	// global is the inverse of org being set: true for a file-based blueprint,
	// false for an org-scoped database blueprint.
	Field("global", queryv1.FieldType_FIELD_TYPE_BOOLEAN,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE).
	Field("created", queryv1.FieldType_FIELD_TYPE_DATETIME,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE,
		queryv1.Operator_OPERATOR_GT, queryv1.Operator_OPERATOR_GTE,
		queryv1.Operator_OPERATOR_LT, queryv1.Operator_OPERATOR_LTE).
	Field("updated", queryv1.FieldType_FIELD_TYPE_DATETIME,
		queryv1.Operator_OPERATOR_EQ, queryv1.Operator_OPERATOR_NE,
		queryv1.Operator_OPERATOR_GT, queryv1.Operator_OPERATOR_GTE,
		queryv1.Operator_OPERATOR_LT, queryv1.Operator_OPERATOR_LTE).
	DefaultSort("name", queryv1.SortDir_SORT_DIR_ASC).
	Build()

// blueprintFieldValues extracts, for each field declared in
// BlueprintsQueryDescriptor, its comparable string value from a
// models.BlueprintSummary and whether the field is actually set.
// BlueprintSummary is already the backend-agnostic view GetBlueprintsSummary
// produces for both file-based and org-scoped database blueprints, so
// matching against it makes QueryBlueprints transparent to where a blueprint
// is actually stored.
var blueprintFieldValues = map[string]func(b *models.BlueprintSummary) (string, bool){
	"name": func(b *models.BlueprintSummary) (string, bool) { return b.Name, b.Name != "" },
	"org":  func(b *models.BlueprintSummary) (string, bool) { return b.Org, b.Org != "" },
	"global": func(b *models.BlueprintSummary) (string, bool) {
		return strconv.FormatBool(b.IsGlobal), true
	},
	"created": func(b *models.BlueprintSummary) (string, bool) {
		if b.CreatedAt.IsZero() {
			return "", false
		}
		return b.CreatedAt.UTC().Format(time.RFC3339), true
	},
	"updated": func(b *models.BlueprintSummary) (string, bool) {
		if b.UpdatedAt.IsZero() {
			return "", false
		}
		return b.UpdatedAt.UTC().Format(time.RFC3339), true
	},
}

func blueprintFieldType(name string) queryv1.FieldType {
	for _, f := range BlueprintsQueryDescriptor.GetFields() {
		if f.GetName() == name {
			return f.GetType()
		}
	}
	return queryv1.FieldType_FIELD_TYPE_UNSPECIFIED
}

// QueryBlueprints returns a summary of every blueprint matching payload, as
// validated against BlueprintsQueryDescriptor. It draws from the same
// backend-agnostic source as GetBlueprintsSummary — the in-memory file-based
// blueprints plus every org-scoped database blueprint, read fresh from the
// store — so a query is transparent to where a matched blueprint is actually
// stored, and applies payload's filters/sort/page over that merged set in
// memory.
func (bm *BlueprintManager) QueryBlueprints(payload *queryv1.Payload) ([]*models.BlueprintSummary, error) {
	if err := query.Validate(BlueprintsQueryDescriptor, payload); err != nil {
		return nil, fmt.Errorf("%w: %s", models.ErrInvalidParameters, err)
	}

	summaries, err := bm.GetBlueprintsSummary()
	if err != nil {
		return nil, err
	}

	filtered := make([]*models.BlueprintSummary, 0, len(summaries))
	for _, b := range summaries {
		matched, err := matchesBlueprintFilters(b, payload.GetFilters())
		if err != nil {
			return nil, fmt.Errorf("%w: %s", models.ErrInvalidParameters, err)
		}
		if matched {
			filtered = append(filtered, b)
		}
	}

	sorts := payload.GetSort()
	if len(sorts) == 0 {
		sorts = BlueprintsQueryDescriptor.GetDefaultSort()
	}
	sortBlueprints(filtered, sorts)

	return pageBlueprints(filtered, payload.GetPage()), nil
}

func matchesBlueprintFilters(b *models.BlueprintSummary, filters *queryv1.Filters) (bool, error) {
	conditions := filters.GetConditions()
	if len(conditions) == 0 {
		return true, nil
	}

	or := filters.GetOp() == queryv1.FilterOp_FILTER_OP_OR
	for _, c := range conditions {
		matched, err := matchesBlueprintCondition(b, c)
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

func matchesBlueprintCondition(b *models.BlueprintSummary, c *queryv1.Condition) (bool, error) {
	extract, ok := blueprintFieldValues[c.GetField()]
	if !ok {
		return false, fmt.Errorf("query: unknown field %q", c.GetField())
	}
	typ := blueprintFieldType(c.GetField())
	value, present := extract(b)

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
			switch typ {
			case queryv1.FieldType_FIELD_TYPE_DATETIME:
				matched, err = matchesBlueprintDatetime(value, queryv1.Operator_OPERATOR_EQ, c.GetValues()[0])
			case queryv1.FieldType_FIELD_TYPE_BOOLEAN:
				matched, err = matchesBlueprintBoolean(value, c.GetValues()[0])
			default:
				matched = blueprintGlobMatch(c.GetValues()[0], value)
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
		return matchesBlueprintDatetime(value, c.GetOp(), c.GetValues()[0])

	default:
		return false, fmt.Errorf("query: unsupported operator %s", c.GetOp())
	}
}

func matchesBlueprintDatetime(fieldValue string, op queryv1.Operator, raw string) (bool, error) {
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

func matchesBlueprintBoolean(fieldValue, raw string) (bool, error) {
	fv, err := strconv.ParseBool(fieldValue)
	if err != nil {
		return false, fmt.Errorf("invalid stored boolean %q: %w", fieldValue, err)
	}
	cv, err := strconv.ParseBool(raw)
	if err != nil {
		return false, err
	}
	return fv == cv, nil
}

// blueprintGlobMatch reports whether value case-insensitively matches
// pattern, using the same "*" (any run of characters) / "?" (any single
// character) wildcard vocabulary as query.v1.Operator's EQ/NE glob support.
func blueprintGlobMatch(pattern, value string) bool {
	return blueprintGlobMatchLower(strings.ToLower(pattern), strings.ToLower(value))
}

func blueprintGlobMatchLower(pattern, value string) bool {
	if pattern == "" {
		return value == ""
	}
	switch pattern[0] {
	case '*':
		if len(pattern) == 1 {
			return true
		}
		for i := 0; i <= len(value); i++ {
			if blueprintGlobMatchLower(pattern[1:], value[i:]) {
				return true
			}
		}
		return false
	case '?':
		if len(value) == 0 {
			return false
		}
		return blueprintGlobMatchLower(pattern[1:], value[1:])
	default:
		if len(value) == 0 || value[0] != pattern[0] {
			return false
		}
		return blueprintGlobMatchLower(pattern[1:], value[1:])
	}
}

func sortBlueprints(list []*models.BlueprintSummary, sorts []*queryv1.Sort) {
	if len(sorts) == 0 {
		return
	}
	sort.SliceStable(list, func(i, j int) bool {
		for _, s := range sorts {
			extract, ok := blueprintFieldValues[s.GetField()]
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

func pageBlueprints(list []*models.BlueprintSummary, page *queryv1.Page) []*models.BlueprintSummary {
	if page == nil {
		return list
	}
	offset := int(page.GetOffset())
	if offset < 0 {
		offset = 0
	}
	if offset > len(list) {
		return []*models.BlueprintSummary{}
	}
	end := len(list)
	if limit := int(page.GetLimit()); limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return list[offset:end]
}
