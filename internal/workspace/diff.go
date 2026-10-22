// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/pmezard/go-difflib/difflib"
	"gopkg.in/yaml.v3"
)

// DiffOp classifies one row of a side-by-side diff.
type DiffOp int

// Row kinds produced by DiffLines.
const (
	DiffEqual   DiffOp = iota // the line is identical on both sides
	DiffRemoved               // the line exists only on the left
	DiffAdded                 // the line exists only on the right
	DiffChanged               // both sides hold a line here and they differ
)

// DiffRow is one aligned row of a side-by-side diff. Line numbers are
// 1-based; 0 means the side has no line in this row.
type DiffRow struct {
	Op        DiffOp
	LeftLine  int
	LeftText  string
	RightLine int
	RightText string
}

// DiffOptions describes the workspace a blueprint is being normalized for.
type DiffOptions struct {
	// WorkspaceName and Organization identify the DNS defaults that
	// Workspace.Values fills in when the blueprint leaves hostname and
	// subdomain unset.
	WorkspaceName string
	Organization  string
	// UserEnvKeys are the names of the user's own environment variables,
	// which Workspace.Values merges into env on top of the blueprint's.
	UserEnvKeys []string
}

// injectedEnvKeys are the env entries Workspace.Values adds on its own,
// independent of the blueprint.
var injectedEnvKeys = []string{
	"PROVISIONER_VERSION", "PROVISION_MODE",
	"GIT_ADDRESS", "GIT_REPOOWNER", "GIT_REPONAME", "GIT_REPOREF",
	"WORKLOAD_NAME", "WORKLOAD_NAMESPACE", "WORKLOAD_KIND",
}

// BlueprintFromValues recovers the blueprint from a workspace's stored Helm
// values. Decoding into models.Blueprint drops everything that is not a
// blueprint field, such as the provisioner-internal "__" keys and the
// per-script "__file" entries.
func BlueprintFromValues(values map[string]interface{}) (*models.Blueprint, error) {
	data, err := yaml.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal values: %w", err)
	}
	var bp models.Blueprint
	if err := yaml.Unmarshal(data, &bp); err != nil {
		return nil, fmt.Errorf("unmarshal blueprint from values: %w", err)
	}
	return &bp, nil
}

// normalizeForDiff removes from bp what Workspace.Values adds around the
// blueprint, so that the same blueprint yields the same document whether it
// was recovered from stored values or freshly evaluated. It returns a copy;
// bp is left untouched.
func normalizeForDiff(bp *models.Blueprint, opts DiffOptions) *models.Blueprint {
	out := *bp
	// Metadata is derived from the provisioning scope, not authored.
	out.Metadata = models.BlueprintMetadata{}

	if len(bp.Env) > 0 {
		out.Env = make(map[string]string, len(bp.Env))
		for k, v := range bp.Env {
			out.Env[k] = v
		}
		for _, k := range injectedEnvKeys {
			delete(out.Env, k)
		}
		for _, k := range opts.UserEnvKeys {
			delete(out.Env, k)
		}
	}

	if out.Hostname != "" && out.Hostname == blueprint.NormalizeDNSLabel(opts.WorkspaceName) {
		out.Hostname = ""
	}
	if out.Subdomain != "" && out.Subdomain == blueprint.NormalizeDNSLabel(opts.Organization) {
		out.Subdomain = ""
	}

	if k8shelldTagOverride != "" {
		repo, _, _ := strings.Cut(out.K8shelld.Image, ":")
		out.K8shelld.Image = repo + ":" + k8shelldTagOverride
	}
	return &out
}

// secretKey matches map keys whose value is treated as a secret.
var secretKey = regexp.MustCompile(`(?i)(secret|token|passw|credential|private|api[_-]?key|access[_-]?key|auth|(^|[_-])key$|(^|[_-])pat$)`)

// secretText matches secret material embedded in free-form strings such as
// init scripts and files: PEM private keys and well-known token formats.
var secretText = regexp.MustCompile(
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----` +
		`|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,}` +
		`|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}`)

// redactor replaces secrets with a marker derived from a per-instance random
// salt, so equal secrets get equal markers within one diff (an unchanged
// secret does not show up as a change, a changed one does) while the marker
// reveals nothing about the value and cannot be matched across diffs.
type redactor struct{ salt []byte }

func newRedactor() (*redactor, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate redaction salt: %w", err)
	}
	return &redactor{salt: salt}, nil
}

func (r *redactor) mark(secret string) string {
	mac := hmac.New(sha256.New, r.salt)
	mac.Write([]byte(secret))
	return "***REDACTED:" + hex.EncodeToString(mac.Sum(nil))[:6] + "***"
}

// walk redacts secrets in the generic tree v in place and returns it.
func (r *redactor) walk(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, e := range t {
			if s, ok := e.(string); ok && s != "" && secretKey.MatchString(k) {
				t[k] = r.mark(s)
				continue
			}
			t[k] = r.walk(e)
		}
		return t
	case []interface{}:
		for i, e := range t {
			t[i] = r.walk(e)
		}
		return t
	case string:
		return secretText.ReplaceAllStringFunc(t, r.mark)
	default:
		return v
	}
}

// renderBlueprintYAML normalizes bp as described on DiffOptions and renders
// it as YAML with all fields present and keys in a stable order, redacting
// secrets with r. Two renderings made with the same redactor are directly
// comparable.
func renderBlueprintYAML(bp *models.Blueprint, opts DiffOptions, r *redactor) (string, error) {
	full, err := toFullValue(reflect.ValueOf(normalizeForDiff(bp, opts)))
	if err != nil {
		return "", fmt.Errorf("convert blueprint: %w", err)
	}
	// The blueprint's own metadata is dropped by normalizeForDiff; leaving
	// its empty shell in the output would only be noise.
	if m, ok := full.(map[string]interface{}); ok {
		delete(m, "metadata")
	}
	data, err := marshalYAML2(r.walk(full))
	if err != nil {
		return "", fmt.Errorf("marshal blueprint: %w", err)
	}
	return string(data), nil
}

// BlueprintDiff is the result of comparing a workspace's blueprint with the
// blueprint it currently resolves to.
type BlueprintDiff struct {
	// WorkspaceYAML and BlueprintYAML are the normalized, redacted
	// documents that were compared.
	WorkspaceYAML string
	BlueprintYAML string
	// Rows is the full documents aligned side by side.
	Rows []DiffRow
	// Identical is true when the two documents are equal.
	Identical               bool
	Added, Removed, Changed int
}

// DiffBlueprints compares deployed, the blueprint recovered from a
// workspace's stored values, with current, the blueprint as it evaluates
// now. Both are normalized per opts and redacted before being compared.
func DiffBlueprints(deployed, current *models.Blueprint, opts DiffOptions) (*BlueprintDiff, error) {
	r, err := newRedactor()
	if err != nil {
		return nil, err
	}
	left, err := renderBlueprintYAML(deployed, opts, r)
	if err != nil {
		return nil, fmt.Errorf("render workspace blueprint: %w", err)
	}
	right, err := renderBlueprintYAML(current, opts, r)
	if err != nil {
		return nil, fmt.Errorf("render current blueprint: %w", err)
	}

	d := &BlueprintDiff{WorkspaceYAML: left, BlueprintYAML: right, Identical: left == right}
	d.Rows = DiffLines(left, right)
	for _, row := range d.Rows {
		switch row.Op {
		case DiffAdded:
			d.Added++
		case DiffRemoved:
			d.Removed++
		case DiffChanged:
			d.Changed++
		}
	}
	return d, nil
}

// DiffLines aligns the lines of left and right into side-by-side rows. A
// run of differing lines is paired up row by row as DiffChanged; whichever
// side is longer contributes the remainder as DiffRemoved or DiffAdded.
func DiffLines(left, right string) []DiffRow {
	a, b := splitLines(left), splitLines(right)
	var rows []DiffRow
	for _, op := range difflib.NewMatcher(a, b).GetOpCodes() {
		switch op.Tag {
		case 'e':
			for k := 0; k < op.I2-op.I1; k++ {
				rows = append(rows, DiffRow{
					Op:       DiffEqual,
					LeftLine: op.I1 + k + 1, LeftText: a[op.I1+k],
					RightLine: op.J1 + k + 1, RightText: b[op.J1+k],
				})
			}
		case 'd', 'i', 'r':
			nl, nr := op.I2-op.I1, op.J2-op.J1
			for k := 0; k < max(nl, nr); k++ {
				row := DiffRow{}
				if k < nl {
					row.LeftLine, row.LeftText = op.I1+k+1, a[op.I1+k]
				}
				if k < nr {
					row.RightLine, row.RightText = op.J1+k+1, b[op.J1+k]
				}
				switch {
				case k < nl && k < nr:
					row.Op = DiffChanged
				case k < nl:
					row.Op = DiffRemoved
				default:
					row.Op = DiffAdded
				}
				rows = append(rows, row)
			}
		}
	}
	return rows
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// AppliedPatchesFromValues returns the policy patches that were applied to a
// workspace's blueprint when it was provisioned, as recorded in its stored
// values. Re-applying them to the current blueprint lets a diff show only
// real blueprint changes. Nothing is evaluated: the patches are replayed
// exactly as recorded. A release that recorded none yields nil.
func AppliedPatchesFromValues(values map[string]interface{}) ([]authz.ProvisionPatch, error) {
	doc, _ := values["__blueprintyaml__"].(string)
	if doc == "" {
		return nil, nil
	}
	var file struct {
		Obligations map[string]string `yaml:"obligations"`
	}
	if err := yaml.Unmarshal([]byte(doc), &file); err != nil {
		return nil, fmt.Errorf("parse stored blueprint file: %w", err)
	}
	patches := make([]authz.ProvisionPatch, 0, len(file.Obligations))
	for path, value := range file.Obligations {
		patches = append(patches, authz.ProvisionPatch{Path: path, Value: value})
	}
	sort.Slice(patches, func(i, j int) bool { return patches[i].Path < patches[j].Path })
	return patches, nil
}
