// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	govalidator "github.com/go-playground/validator/v10"
	"github.com/k8shell-io/common/pkg/models"
	commonvalidator "github.com/k8shell-io/common/pkg/validator"
	"github.com/k8shell-io/yaml-cel/pkg/yamlcel"
	"gopkg.in/yaml.v3"
)

// celDisplayPrefix is prepended to a CEL expression's source when
// cloneAndProcessCELNodes converts a real `!cel` tagged node into a plain
// display string, e.g. for GetBlueprint/GetRawBlueprint output. Callers that
// round-trip a blueprint (fetch, edit, submit for validation) send this same
// convention back, so it must be reversed before CEL evaluation.
const celDisplayPrefix = "!cel:"

// ValidationIssue is a single problem found while validating a raw blueprint
// YAML document. Line and Column are 1-indexed positions in the submitted
// YAML; both are 0 when the underlying check does not report a position
// (e.g. semantic checks that operate on the decoded blueprint rather than
// the YAML source).
type ValidationIssue struct {
	Line    int
	Column  int
	Field   string
	Message string
}

var yamlErrorLineRE = regexp.MustCompile(`line (\d+)`)
var unknownFieldRE = regexp.MustCompile(`field (\S+) not found`)

// yamlcel's Eval/EvalToBytes build a dotted path (e.g. "k8shelld.image") as
// it walks the template and embeds it in the wrapped error text:
// evalCELValueWithPath does fmt.Errorf("failed to evaluate '%s': %w", path,
// err), and evalCELNodeWithPath/parseCELValue similarly wrap with the path.
// These extract that path so a CEL failure points at the actual expression
// field instead of the generic fieldCEL sentinel. A list index already
// comes through bracketed (e.g. "network.allowEgressToCIDRs[0]"), matching
// this codebase's own convention, but a map key (e.g. a storage name) comes
// through as a plain dotted segment ("storages.shared...") since yaml-cel
// has no notion of which fields are user-keyed maps — bracketMapKeys fixes
// that up before the path is used as a Field.
var (
	celEvalPathRE  = regexp.MustCompile(`failed to evaluate '([^']*)'`)
	celValuePathRE = regexp.MustCompile(`(?:empty CELValue at path|failed to parse value for key) '([^']*)'`)
	celSectionRE   = regexp.MustCompile(`section '([^']*)'`)
)

// fieldYAML and fieldCEL are used for structural issues that aren't tied to
// a single blueprint field (YAML syntax errors) or where a CEL evaluation
// failure's error text doesn't name a path at all. Every ValidationIssue
// carries a non-empty Field so callers never have to special-case a missing
// one.
const (
	fieldYAML = "yaml"
	fieldCEL  = "cel"
)

// mapFieldPrefixes lists the dotted prefixes of blueprint fields that are
// user-keyed maps (Storages, Podman.Storages, Env, Apps, ExtFiles,
// Podman.ExtFiles), checked longest-first so "podman.storages" is matched
// before the "storages" it would otherwise collide with. claimSpec is
// deliberately excluded even though it's also a map in the Go model: its
// keys are fixed PersistentVolumeClaimSpec field names (resources,
// storageClassName, ...), not user-chosen identifiers, so every other
// Field this package reports already addresses it with plain dots (e.g.
// "storages[home].claimSpec.resources.requests").
var mapFieldPrefixes = []string{
	"podman.storages",
	"podman.extFiles",
	"storages",
	"extFiles",
	"apps",
	"env",
}

// bracketMapKeys rewrites the first user-keyed map segment in path (per
// mapFieldPrefixes) from yaml-cel's plain-dot form ("storages.shared...")
// into this codebase's bracketed convention ("storages[shared]..."), so a
// CEL error's Field matches every other issue's format (see
// TestValidateRawBlueprintFieldPathFormat) instead of needing a second
// parser downstream.
func bracketMapKeys(path string) string {
	for _, prefix := range mapFieldPrefixes {
		rest, ok := strings.CutPrefix(path, prefix+".")
		if !ok {
			continue
		}
		key, tail, hasTail := strings.Cut(rest, ".")
		if hasTail {
			return fmt.Sprintf("%s[%s].%s", prefix, key, tail)
		}
		return fmt.Sprintf("%s[%s]", prefix, key)
	}
	return path
}

// celErrorField recovers the dotted/bracketed field path from a
// yamlcel evaluation error, falling back to the enclosing section name and
// finally to the generic fieldCEL sentinel if the message names neither.
func celErrorField(err error) string {
	msg := err.Error()
	if m := celEvalPathRE.FindStringSubmatch(msg); m != nil {
		return bracketMapKeys(m[1])
	}
	if m := celValuePathRE.FindStringSubmatch(msg); m != nil {
		return bracketMapKeys(m[1])
	}
	if m := celSectionRE.FindStringSubmatch(msg); m != nil {
		return bracketMapKeys(m[1])
	}
	return fieldCEL
}

// structValidator validates models.Blueprint the same way bp.Validate()
// does (same "validate" tags and custom validators), but reports each
// error's field as a fully-qualified, dot-separated path from the blueprint
// root using JSON tag names (e.g. "network.allowEgressToCIDRs[0]") instead
// of Go struct field names, so a frontend can attach it to the right form
// field without any name translation of its own.
var structValidator = newStructValidator()

func newStructValidator() *govalidator.Validate {
	v := govalidator.New()
	commonvalidator.RegisterCustomValidators(v)
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}

// structFieldIssues validates bp's own "validate" struct tags, returning one
// ValidationIssue per failure with Field set to the full JSON-tag path
// (namespace, with the leading root type name stripped).
func structFieldIssues(bp *models.Blueprint) []ValidationIssue {
	err := structValidator.Struct(bp)
	if err == nil {
		return nil
	}

	verrs, ok := err.(govalidator.ValidationErrors)
	if !ok {
		return []ValidationIssue{{Field: fieldYAML, Message: err.Error()}}
	}

	issues := make([]ValidationIssue, 0, len(verrs))
	for _, fe := range verrs {
		field := fe.Namespace()
		if idx := strings.IndexByte(field, '.'); idx >= 0 {
			field = field[idx+1:]
		}
		issues = append(issues, ValidationIssue{
			Field:   field,
			Message: structValidationMessage(fe),
		})
	}
	return issues
}

// structValidationMessage mirrors k8shell-io/common's pkg/validator message
// style (same wording), keyed off the leaf field name so it stays readable.
func structValidationMessage(fe govalidator.FieldError) string {
	field := fe.Field()
	value := fmt.Sprintf("%v", fe.Value())
	switch fe.Tag() {
	case "required":
		return fmt.Sprintf("%s is required", field)
	case "oneof":
		return fmt.Sprintf("%s must be one of: %s (value: %s)", field, strings.ReplaceAll(fe.Param(), " ", ", "), value)
	case "min":
		return fmt.Sprintf("%s must be at least %s characters long (value: %s)", field, fe.Param(), value)
	case "max":
		return fmt.Sprintf("%s must be no more than %s characters long (value: %s)", field, fe.Param(), value)
	case "len":
		return fmt.Sprintf("%s must be exactly %s characters long (value: %s)", field, fe.Param(), value)
	case "fqdn":
		return fmt.Sprintf("%s must be a valid domain name (value: %s)", field, value)
	case "uri":
		return fmt.Sprintf("%s must be a valid URI (value: %s)", field, value)
	case "startswith":
		return fmt.Sprintf("%s must start with '%s' (value: %s)", field, fe.Param(), value)
	case "required_if":
		return fmt.Sprintf("%s is required when %s (value: %s)", field, fe.Param(), value)
	default:
		return fmt.Sprintf("%s is invalid (validation: %s) (value: %s)", field, fe.Tag(), value)
	}
}

// ValidateRawBlueprint validates a standalone blueprint YAML document without
// registering it in the manager. If the document references an existing
// template via `template:`, that template's already-resolved definition is
// merged in exactly as it would be for a blueprint loaded from disk.
//
// The second return value is the raw (unevaluated) result of that merge —
// the same shape GetRawBlueprint returns for an already-registered
// blueprint, but for this standalone document — so an editor can preview
// the full inherited blueprint as the "own" fields are added, changed, or
// removed. It is only populated when the submission is valid (the first
// return value is empty): a caller should fix the reported issues first
// rather than being handed a preview built from an invalid document.
func (bm *BlueprintManager) ValidateRawBlueprint(data []byte) ([]ValidationIssue, interface{}, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return []ValidationIssue{yamlErrorToIssue(err)}, nil, nil
	}

	node, err := bm.resolveValidationNode(&doc)
	if err != nil {
		return []ValidationIssue{{Field: fieldYAML, Message: err.Error()}}, nil, nil
	}
	node = restoreCELNodes(node)

	var bpData map[string]interface{}
	if err := node.Decode(&bpData); err != nil {
		bpData = make(map[string]interface{})
	}

	mergedNode := node
	if templateName, ok := bpData["template"].(string); ok && templateName != "" {
		bm.mu.RLock()
		parent, exists := bm.rawBlueprints[templateName]
		bm.mu.RUnlock()
		if !exists {
			return []ValidationIssue{{
				Field:   "template",
				Message: fmt.Sprintf("template %q not found", templateName),
				Line:    findKeyLine(node, "template"),
			}}, nil, nil
		}

		mergedNode, err = bm.mergeYAMLNodes(parent.Node, node)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to merge template %q: %w", templateName, err)
		}
	}

	var tmpl yamlcel.CELTemplate
	if err := mergedNode.Decode(&tmpl); err != nil {
		return []ValidationIssue{{Field: fieldYAML, Message: fmt.Sprintf("failed to parse CEL template: %v", err)}}, nil, nil
	}

	name, _ := bpData["name"].(string)
	scope := TestScope()
	scope.Metadata.Name = NormalizeDNSLabel(name)

	mapScope, err := scope.ToMap()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert scope to map: %w", err)
	}
	mapScope["blueprint"] = name

	docBytes, err := tmpl.EvalToBytes(mapScope, map[string]string{})
	if err != nil {
		return []ValidationIssue{{Field: celErrorField(err), Message: fmt.Sprintf("CEL evaluation failed: %v", err)}}, nil, nil
	}

	var preDecodeIssues []ValidationIssue
	for _, e := range validateStorageOwnerIDs(docBytes) {
		preDecodeIssues = append(preDecodeIssues, errorToIssue(e))
	}

	var bp models.Blueprint
	decoder := yaml.NewDecoder(bytes.NewReader(docBytes))
	decoder.KnownFields(bm.knownFields)

	issues := preDecodeIssues
	if err := decoder.Decode(&bp); err != nil {
		// A *yaml.TypeError (unknown field, type mismatch, ...) still leaves
		// every other field decoded into bp, so fold its issues in and keep
		// validating instead of hiding every other problem behind it. Any
		// other decode error means bp is unusable, so bail out with just that.
		var typeErr *yaml.TypeError
		if !errors.As(err, &typeErr) {
			return append(issues, decodeErrorToIssues(err)...), nil, nil
		}
		issues = append(issues, decodeErrorToIssues(err)...)
	}

	issues = append(issues, structFieldIssues(&bp)...)
	for _, e := range validateClaimSpecs(&bp) {
		issues = append(issues, errorToIssue(e))
	}
	for _, e := range validateStorageSizeLimits(&bp) {
		issues = append(issues, errorToIssue(e))
	}
	for _, e := range validateResourceQuantities(&bp) {
		issues = append(issues, errorToIssue(e))
	}
	for _, e := range validateEnvNames(&bp) {
		issues = append(issues, errorToIssue(e))
	}
	for _, e := range validateSecurityContexts(&bp) {
		issues = append(issues, errorToIssue(e))
	}
	for _, e := range validateDescriptionRequired(&bp) {
		issues = append(issues, errorToIssue(e))
	}

	if len(issues) > 0 {
		return issues, nil, nil
	}

	resolved, err := bm.decodeRawNode(mergedNode)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode merged blueprint: %w", err)
	}

	return nil, resolved, nil
}

// errorToIssue converts an error from one of the blueprint.go semantic
// checks (validateClaimSpecs, validateStorageSizeLimits, ...) into a
// ValidationIssue, recovering the field path from *fieldError when present.
func errorToIssue(err error) ValidationIssue {
	issue := ValidationIssue{Message: err.Error()}
	if fe, ok := err.(*fieldError); ok {
		issue.Field = fe.Field()
	}
	return issue
}

// resolveValidationNode unwraps a document node down to the mapping node
// that describes the blueprint itself, following the same `blueprint:`
// unwrapping rule as extractFromMapping.
func (bm *BlueprintManager) resolveValidationNode(root *yaml.Node) (*yaml.Node, error) {
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		return bm.resolveValidationNode(root.Content[0])
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a YAML mapping, got %v", root.Kind)
	}
	if bp := bm.findChildNode(root, "blueprint"); bp != nil {
		return bp, nil
	}
	return root, nil
}

// restoreCELNodes recursively clones node, turning any scalar string value
// with the "!cel:" display prefix (as produced by cloneAndProcessCELNodes)
// back into a properly `!cel`-tagged node so yamlcel.CELTemplate evaluates
// it instead of treating it as a literal string. Nodes that already carry a
// real `!cel` tag (e.g. copied verbatim from a blueprint file) pass through
// unchanged.
func restoreCELNodes(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}

	cloned := &yaml.Node{
		Kind:        node.Kind,
		Style:       node.Style,
		Tag:         node.Tag,
		Value:       node.Value,
		Anchor:      node.Anchor,
		Alias:       node.Alias,
		HeadComment: node.HeadComment,
		LineComment: node.LineComment,
		FootComment: node.FootComment,
		Line:        node.Line,
		Column:      node.Column,
	}

	if node.Kind == yaml.ScalarNode && strings.HasPrefix(node.Value, celDisplayPrefix) {
		cloned.Tag = "!cel"
		cloned.Value = strings.TrimPrefix(node.Value, celDisplayPrefix)
	}

	if len(node.Content) > 0 {
		cloned.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			cloned.Content[i] = restoreCELNodes(child)
		}
	}

	return cloned
}

// findKeyLine returns the source line of the given top-level key in a
// mapping node, or the node's own line if the key isn't found.
func findKeyLine(node *yaml.Node, key string) int {
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				return node.Content[i].Line
			}
		}
	}
	return node.Line
}

// parseYAMLErrorLine extracts a 1-indexed line number from a yaml.v3 error
// message such as "line 12: field foo not found in type models.Blueprint".
func parseYAMLErrorLine(msg string) int {
	m := yamlErrorLineRE.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func yamlErrorToIssue(err error) ValidationIssue {
	return ValidationIssue{Field: fieldYAML, Message: err.Error(), Line: parseYAMLErrorLine(err.Error())}
}

// parseYAMLErrorField extracts the offending field name from a yaml.v3
// "field X not found in type ..." message, falling back to the generic
// fieldYAML for messages that don't name a field (e.g. type mismatches).
func parseYAMLErrorField(msg string) string {
	if m := unknownFieldRE.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return fieldYAML
}

// decodeErrorToIssues splits a yaml.v3 decode error into one ValidationIssue
// per underlying message, extracting a line number and field from each when present.
func decodeErrorToIssues(err error) []ValidationIssue {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		issues := make([]ValidationIssue, 0, len(typeErr.Errors))
		for _, m := range typeErr.Errors {
			issues = append(issues, ValidationIssue{Message: m, Line: parseYAMLErrorLine(m), Field: parseYAMLErrorField(m)})
		}
		return issues
	}
	return []ValidationIssue{yamlErrorToIssue(err)}
}
