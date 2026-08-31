// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package blueprint loads, validates, and evaluates workspace blueprint
// definitions. Blueprints are YAML documents that describe how a k8shell
// workspace pod should be configured. They support CEL template expressions,
// template inheritance (parent/child overriding), and hot-reloading via a
// filesystem watcher so changes take effect without restarting the server.
package blueprint

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"github.com/k8shell-io/common/pkg/config"
	log "github.com/k8shell-io/common/pkg/logger"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/yaml-cel/pkg/yamlcel"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// RawBlueprint represents an unprocessed blueprint with CEL expressions intact.
type RawBlueprint struct {
	Name             string
	Description      string
	Template         string
	IsTemplate       bool
	SourceFile       string
	Node             *yaml.Node // fully merged (own + inherited) content
	OwnNode          *yaml.Node // content defined directly on this blueprint, before merging with Template
	InheritanceChain []string   // ordered list of blueprint names from root ancestor to this blueprint

	// CreatedAt and UpdatedAt record when the blueprint was first registered
	// and last changed. For a file-based blueprint both are set to the source
	// file's last-modified time (a file carries no separate creation record);
	// for an org blueprint loaded via OrgBlueprintStore they are the database
	// row's timestamps.
	CreatedAt time.Time
	UpdatedAt time.Time

	// Org is non-empty for a blueprint loaded from the database via
	// OrgBlueprintStore (see orgstore.go), naming the organization it is
	// scoped to. Empty for a file-based blueprint. An org blueprint is keyed
	// in BlueprintManager.rawBlueprints under orgBlueprintKey(Org, Name)
	// rather than its bare Name, so it can coexist with a file-based
	// blueprint of the same name.
	Org string
}

// BlueprintScope holds the runtime context passed to CEL template evaluation.
// It carries the authenticated user, the target workspace name, and blueprint
// metadata (repo coordinates, resolved blueprint name) so templates can
// conditionally configure resources per-user or per-repository.
type BlueprintScope struct {
	User          *models.User              `yaml:"user"`
	WorkspaceName string                    `yaml:"workspaceName"`
	Metadata      *models.BlueprintMetadata `yaml:"metadata"`
}

// ToMap serialises the scope to a plain map[string]any for CEL evaluation.
func (bs *BlueprintScope) ToMap() (map[string]any, error) {
	data, err := yaml.Marshal(bs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal BlueprintScope: %w", err)
	}

	var result map[string]any
	if err := yaml.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal to map: %w", err)
	}

	return result, nil
}

var ErrBlueprintNotFound = errors.New("blueprint not found")

// requiredCaps are the capabilities that k8shelld requires to function properly.
var requiredCaps = []corev1.Capability{"CHOWN", "SETUID", "SETGID"}

// MergeStrategies allow custom list merging strategies per dotted path.
type MergeStrategies map[string]func(dst, src []interface{}) []interface{}

// LoadOptions contains configuration for loading blueprints.
type LoadOptions struct {
	Dir         string
	Strategies  MergeStrategies
	EnableWatch bool

	// OrgStore, when set, supplies org-scoped blueprint definitions from the
	// database that are merged with the file-based blueprints loaded from
	// Dir. Nil disables org blueprint support entirely.
	OrgStore OrgBlueprintStore
}

// BlueprintManager manages blueprints with lazy CEL evaluation.
type BlueprintManager struct {
	log           *zerolog.Logger          // Logger for the blueprint manager
	rawBlueprints map[string]*RawBlueprint // Map of blueprint names to their raw definitions
	knownFields   bool                     // Whether to allow unknown fields in YAML decoding
	strategies    MergeStrategies          // Custom strategies for merging lists in blueprints
	processor     *config.Processor        // YAML processor for parsing and validating blueprints
	watcher       *Watcher                 // the file watcher
	orgStore      OrgBlueprintStore        // optional database-backed store of org-scoped blueprints
	mu            sync.RWMutex             // Mutex for thread-safe access to rawBlueprints
}

// TestScope creates a minimal BlueprintScope for testing purposes.
func TestScope() *BlueprintScope {
	return &BlueprintScope{
		Metadata: &models.BlueprintMetadata{
			Name:        "testblueprint",
			RepoName:    "testrepo",
			RepoOwner:   "testowner",
			RepoRef:     "testref",
			RepoAddress: "testaddress",
		},
		User: &models.User{
			Username:   "testuser",
			IsValid:    true,
			ExpiresAt:  time.Now().Add(24 * time.Hour),
			UID:        1000,
			GID:        1000,
			Fullname:   "Test User",
			Email:      "testuser@example.com",
			Password:   "testpassword",
			Locked:     false,
			Roles:      []models.Role{"role1", "role2"},
			Blueprints: []string{"testblueprint"},
			Source:     "testsource",
		},
	}
}

// NewBlueprintManager creates a new blueprint manager.
func NewBlueprintManager(opts LoadOptions) (*BlueprintManager, error) {
	if opts.Strategies == nil {
		opts.Strategies = MergeStrategies{}
	}

	bm := &BlueprintManager{
		log:           log.NewLogger("blueprint"),
		rawBlueprints: make(map[string]*RawBlueprint),
		knownFields:   true,
		strategies:    opts.Strategies,
		processor: config.NewProcessor(config.ProcessorOptions{
			EnableEnvVarExpansion: false,
			EnableFileTag:         true,
		}),
		orgStore: opts.OrgStore,
		mu:       sync.RWMutex{},
	}

	if opts.EnableWatch {
		bm.watcher = NewWatcher(opts.Dir, 500*time.Millisecond, func() error {
			return bm.loadAndValidateBlueprints()
		})

		if err := bm.loadAndValidateBlueprints(); err != nil {
			return nil, fmt.Errorf("initial load failed: %w", err)
		}

		err := bm.watcher.Setup()
		if err != nil {
			return nil, fmt.Errorf("failed to setup file watcher: %w", err)
		}
	}

	bm.log.Info().Msgf("Loaded %d blueprints from %s, watch enabled: %v", len(bm.rawBlueprints),
		opts.Dir, bm.watcher != nil)
	return bm, nil
}

// loadAndValidateBlueprints loads and validates all blueprints atomically
func (bm *BlueprintManager) loadAndValidateBlueprints() (err error) {
	bm.mu.Lock()
	originalBlueprints := bm.rawBlueprints
	bm.rawBlueprints = make(map[string]*RawBlueprint)
	bm.mu.Unlock()

	defer func() {
		if err != nil {
			bm.mu.Lock()
			bm.rawBlueprints = originalBlueprints
			bm.mu.Unlock()
		} else {
			bm.log.Info().Msg("Successfully loaded and validated blueprints")
		}
	}()

	if err = bm.loadRawBlueprints(bm.watcher.watchDir); err != nil {
		return fmt.Errorf("failed to load blueprints: %w", err)
	}

	if err = bm.loadOrgBlueprints(); err != nil {
		return fmt.Errorf("failed to load org blueprints: %w", err)
	}

	if err = bm.resolveInheritance(); err != nil {
		return fmt.Errorf("failed to resolve inheritance: %w", err)
	}

	if errs := bm.validateAllBlueprints(); len(errs) > 0 {
		out := ""
		for _, e := range errs {
			out += fmt.Sprintf("%s\n", e.Error())
		}
		err = fmt.Errorf("failed to validate blueprint:\n%s", out)
		return err
	}

	return nil
}

// validateAllBlueprints validates all loaded blueprints by checking CEL template syntax
func (bm *BlueprintManager) validateAllBlueprints() []error {
	validationScope := TestScope()

	var allErrors []error

	bm.mu.RLock()
	rawBlueprints := make([]*RawBlueprint, 0, len(bm.rawBlueprints))
	for _, rawBp := range bm.rawBlueprints {
		rawBlueprints = append(rawBlueprints, rawBp)
	}
	bm.mu.RUnlock()

	for _, rawBp := range rawBlueprints {
		name := rawBp.Name
		bp, err := bm.evaluateRawBlueprint(rawBp, name, validationScope)
		if err != nil {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %w", name, err))
			continue
		}
		v := bp.Validate()
		if v != nil {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %v", name, v))
		}
		for _, e := range validateClaimSpecs(bp) {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %w", name, e))
		}
		for _, e := range validateStorageSizeLimits(bp) {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %w", name, e))
		}
		for _, e := range validateResourceQuantities(bp) {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %w", name, e))
		}
		for _, e := range validateEnvNames(bp) {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %w", name, e))
		}
		for _, e := range validateSecurityContexts(bp) {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %w", name, e))
		}
		for _, e := range validateDescriptionRequired(bp) {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %w", name, e))
		}
	}

	return allErrors
}

// fieldError pairs a validation message with the dotted blueprint field path
// it applies to (e.g. "resources.cpu", "storages[home].sizeLimit"), so callers
// that need structured output (ValidateRawBlueprint) can report a field
// without re-parsing the message text. Field is deliberately not part of the
// error message itself, matching how go-playground/validator's FieldError
// separates the two.
type fieldError struct {
	field   string
	message string
}

func (e *fieldError) Error() string { return e.message }

// Field returns the dotted blueprint field path the error applies to.
func (e *fieldError) Field() string { return e.field }

func newFieldError(field, format string, args ...interface{}) error {
	return &fieldError{field: field, message: fmt.Sprintf(format, args...)}
}

// RequireDescription controls whether validateDescriptionRequired rejects a
// blueprint with an empty or missing description. Disabled by default, so
// blueprints without one are allowed for now; flip to true once every
// blueprint in use has been given one. Description is never inherited from
// a Template regardless of this switch — see the "description" exclusion in
// mergeYAMLNodesWithTags (resolve.go).
var RequireDescription = false

// validateDescriptionRequired checks that bp.Description is set. Kept as a
// provisioner-local check rather than a "required" tag on the shared
// models.Blueprint.Description field so RequireDescription can toggle it at
// runtime; a struct tag can't be.
func validateDescriptionRequired(bp *models.Blueprint) []error {
	if RequireDescription && bp.Description == "" {
		return []error{newFieldError("description", "description is required")}
	}
	return nil
}

// envNameRE matches a POSIX-conformant environment variable name: a letter
// or underscore, followed by any number of letters, digits, or underscores.
var envNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateEnvNames checks that every key in the blueprint's env map is a
// valid environment variable name. models.Blueprint.Env carries no validate
// tag on its keys, so a value like "TEST X" would otherwise pass.
func validateEnvNames(bp *models.Blueprint) []error {
	var errs []error
	for name := range bp.Env {
		if !envNameRE.MatchString(name) {
			errs = append(errs, newFieldError(fmt.Sprintf("env[%s]", name), "env %q is not a valid environment variable name", name))
		}
	}
	return errs
}

// validateResourceQuantities checks that every CPU/memory limit on the
// blueprint is a valid Kubernetes resource quantity (e.g. "500m", "2",
// "512Mi"). The "required" struct tag on models.Resources only rejects an
// empty string, so a malformed value like "4ddsfsdf" would otherwise pass.
func validateResourceQuantities(bp *models.Blueprint) []error {
	type namedQuantity struct {
		name  string
		value string
	}

	quantities := []namedQuantity{
		{"resources.cpu", bp.Resources.CPU},
		{"resources.memory", bp.Resources.Memory},
		{"podman.resources.cpu", bp.Podman.Resources.CPU},
		{"podman.resources.memory", bp.Podman.Resources.Memory},
	}

	var errs []error
	for _, q := range quantities {
		if q.value == "" {
			continue
		}
		if _, err := resource.ParseQuantity(q.value); err != nil {
			errs = append(errs, newFieldError(q.name, "%s: %q is not a valid Kubernetes quantity: %v", q.name, q.value, err))
		}
	}
	return errs
}

// validateStorageSizeLimits checks that sizeLimit is only specified on emptyDir and memory
// storage types, and that its value is a valid Kubernetes resource quantity.
func validateStorageSizeLimits(bp *models.Blueprint) []error {
	type namedStorage struct {
		name    string // display name, e.g. "home" or "podman.home"
		path    string // field path, e.g. "storages[home]" or "podman.storages[home]"
		storage models.Storage
	}

	var all []namedStorage
	for name, s := range bp.Storages {
		all = append(all, namedStorage{name: name, path: fmt.Sprintf("storages[%s]", name), storage: s})
	}
	for name, s := range bp.Podman.Storages {
		all = append(all, namedStorage{name: "podman." + name, path: fmt.Sprintf("podman.storages[%s]", name), storage: s})
	}

	var errs []error
	for _, ns := range all {
		s := ns.storage
		if !s.Enabled || s.SizeLimit == "" {
			continue
		}
		storageType := s.Type
		if storageType == "" {
			storageType = "local"
		}
		switch storageType {
		case "emptyDir", "memory":
			if _, err := resource.ParseQuantity(s.SizeLimit); err != nil {
				errs = append(errs, newFieldError(ns.path+".sizeLimit",
					"storage %q: sizeLimit %q is not a valid Kubernetes quantity: %v", ns.name, s.SizeLimit, err))
			}
		default:
			errs = append(errs, newFieldError(ns.path+".sizeLimit",
				"storage %q: sizeLimit is only valid for emptyDir and memory types, got type %q", ns.name, storageType))
		}
	}
	return errs
}

// validateStorageOwnerIDs checks that fsOwnerUid/fsOwnerGid on every storage
// entry (workspace and podman) are valid integers, using docBytes — the
// fully CEL-evaluated document, decoded generically — rather than the typed
// models.Blueprint. Storage.FsOwnerUid/FsOwnerGid are *int fields, so a
// non-numeric value (e.g. "abc") fails yaml.v3's struct decode with a
// generic "cannot unmarshal" TypeError that carries no field path; worse,
// yaml.v3 still leaves the pointer allocated and pointing at zero rather
// than nil, making the bad input indistinguishable from a legitimately-set
// "fsOwnerUid: 0" once decoded into bp. Checking the raw, still-intact value
// here reports a properly field-pathed issue instead.
func validateStorageOwnerIDs(docBytes []byte) []error {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(docBytes, &raw); err != nil {
		// Already reported by the main struct decode; nothing more to add.
		return nil
	}

	var errs []error
	errs = append(errs, checkStorageOwnerIDs("storages", raw["storages"])...)
	if podman, ok := raw["podman"].(map[string]interface{}); ok {
		errs = append(errs, checkStorageOwnerIDs("podman.storages", podman["storages"])...)
	}
	return errs
}

// checkStorageOwnerIDs validates fsOwnerUid/fsOwnerGid across every entry of
// a raw storages map (already-decoded generic YAML, keyed by storage name).
func checkStorageOwnerIDs(pathPrefix string, storages interface{}) []error {
	m, ok := storages.(map[string]interface{})
	if !ok {
		return nil
	}

	var errs []error
	for name, raw := range m {
		storage, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		for _, key := range [...]string{"fsOwnerUid", "fsOwnerGid"} {
			v, present := storage[key]
			if !present || v == nil {
				continue
			}
			if !isYAMLInteger(v) {
				field := fmt.Sprintf("%s[%s].%s", pathPrefix, name, key)
				errs = append(errs, newFieldError(field, "%s: %v is not a valid integer", field, v))
			}
		}
	}
	return errs
}

// isYAMLInteger reports whether v is one of the integer types yaml.v3
// produces when decoding a scalar into interface{} (a non-numeric value
// decodes to string instead, a fractional one to float64).
func isYAMLInteger(v interface{}) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	default:
		return false
	}
}

// validateClaimSpecs decodes each storage claimSpec into corev1.PersistentVolumeClaimSpec
// to catch structural errors early, before any Kubernetes API call is made.
func validateClaimSpecs(bp *models.Blueprint) []error {
	type namedStorage struct {
		name    string // display name, e.g. "home" or "podman.home"
		path    string // field path, e.g. "storages[home]" or "podman.storages[home]"
		storage models.Storage
	}

	var all []namedStorage
	for name, s := range bp.Storages {
		all = append(all, namedStorage{name: name, path: fmt.Sprintf("storages[%s]", name), storage: s})
	}
	for name, s := range bp.Podman.Storages {
		all = append(all, namedStorage{name: "podman." + name, path: fmt.Sprintf("podman.storages[%s]", name), storage: s})
	}

	var errs []error
	for _, ns := range all {
		if ns.storage.ClaimSpec == nil {
			continue
		}
		jsonRaw, err := json.Marshal(ns.storage.ClaimSpec)
		if err != nil {
			errs = append(errs, newFieldError(ns.path+".claimSpec", "storage %q: failed to marshal claimSpec: %v", ns.name, err))
			continue
		}
		// Strict decoding (DisallowUnknownFields) so a typo'd claimSpec
		// field (e.g. "storageClassNamex") is reported instead of silently
		// dropped, leaving spec's corresponding field at its zero value —
		// the same failure mode fixed for securityContext below.
		var spec corev1.PersistentVolumeClaimSpec
		dec := json.NewDecoder(bytes.NewReader(jsonRaw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			errs = append(errs, newFieldError(ns.path+".claimSpec", "storage %q: invalid claimSpec: %v", ns.name, err))
		}

		errs = append(errs, validateClaimSpecResourceNames(ns.path, ns.storage.ClaimSpec)...)
	}
	return errs
}

// validPVCResourceNames is the only resource name Kubernetes accepts under a
// PersistentVolumeClaimSpec's resources.requests/limits: "storage".
// PersistentVolumeClaimSpec.Resources (VolumeResourceRequirements) types
// Requests/Limits as plain map[ResourceName]resource.Quantity, so a typo'd
// key like "storagex" decodes without error — DisallowUnknownFields only
// rejects unrecognized struct fields, never arbitrary map keys — and would
// otherwise only be caught once the Kubernetes API server rejects the PVC
// at apply time.
var validPVCResourceNames = map[string]bool{"storage": true}

// validateClaimSpecResourceNames checks every key under claimSpec's
// resources.requests/resources.limits against validPVCResourceNames.
// claimSpec is the raw, undecoded map (models.Storage.ClaimSpec), inspected
// directly since VolumeResourceRequirements' own struct decode can't catch
// an invalid map key.
func validateClaimSpecResourceNames(path string, claimSpec map[string]interface{}) []error {
	resources, ok := claimSpec["resources"].(map[string]interface{})
	if !ok {
		return nil
	}

	var errs []error
	for _, section := range [...]string{"requests", "limits"} {
		entries, ok := resources[section].(map[string]interface{})
		if !ok {
			continue
		}
		for name := range entries {
			if !validPVCResourceNames[name] {
				field := fmt.Sprintf("%s.claimSpec.resources.%s", path, section)
				errs = append(errs, newFieldError(field,
					"%s: %q is not a valid resource name for a PersistentVolumeClaim (expected \"storage\")", field, name))
			}
		}
	}
	return errs
}

// validCapabilityNames is the set of Linux capability names Kubernetes'
// SecurityContext.Capabilities accepts (without the kernel's "CAP_" prefix),
// per capabilities(7). "ALL" is a wildcard token recognized by container
// runtimes for drop (and, less commonly, add) rather than an actual
// capability, and is accepted here for the same reason the required-caps
// check below already treats "drop: [ALL]" specially.
var validCapabilityNames = map[corev1.Capability]bool{
	"ALL": true,

	"AUDIT_CONTROL": true, "AUDIT_READ": true, "AUDIT_WRITE": true,
	"BLOCK_SUSPEND": true, "BPF": true, "CHECKPOINT_RESTORE": true,
	"CHOWN": true, "DAC_OVERRIDE": true, "DAC_READ_SEARCH": true,
	"FOWNER": true, "FSETID": true,
	"IPC_LOCK": true, "IPC_OWNER": true,
	"KILL":  true,
	"LEASE": true, "LINUX_IMMUTABLE": true,
	"MAC_ADMIN": true, "MAC_OVERRIDE": true, "MKNOD": true,
	"NET_ADMIN": true, "NET_BIND_SERVICE": true, "NET_BROADCAST": true, "NET_RAW": true,
	"PERFMON": true,
	"SETFCAP": true, "SETGID": true, "SETPCAP": true, "SETUID": true,
	"SYS_ADMIN": true, "SYS_BOOT": true, "SYS_CHROOT": true, "SYS_MODULE": true,
	"SYS_NICE": true, "SYS_PACCT": true, "SYS_PTRACE": true, "SYS_RAWIO": true,
	"SYS_RESOURCE": true, "SYS_TIME": true, "SYS_TTY_CONFIG": true,
	"SYSLOG":     true,
	"WAKE_ALARM": true,
}

// validateCapabilityNames reports an issue for each entry in caps.Add/Drop
// that isn't a real Linux capability name (or the "ALL" wildcard), catching
// typos like "SYS_PTRACEx" that corev1.Capability's plain string type can't
// reject on its own. fieldPrefix is the dotted path to caps itself (e.g.
// "securityContext.capabilities").
func validateCapabilityNames(fieldPrefix string, caps *corev1.Capabilities) []error {
	if caps == nil {
		return nil
	}
	var errs []error
	for _, c := range caps.Add {
		if !validCapabilityNames[c] {
			errs = append(errs, newFieldError(fieldPrefix+".add", "%s.add: %q is not a valid Linux capability", fieldPrefix, c))
		}
	}
	for _, c := range caps.Drop {
		if !validCapabilityNames[c] {
			errs = append(errs, newFieldError(fieldPrefix+".drop", "%s.drop: %q is not a valid Linux capability", fieldPrefix, c))
		}
	}
	return errs
}

// decodeSecurityContextStrict decodes jsonRaw into a corev1.SecurityContext,
// rejecting any field that doesn't exist on that type. A plain
// json.Unmarshal silently ignores unknown fields (e.g. a typo'd
// "capabilitiesx" instead of "capabilities"), leaving spec zeroed out
// instead of reporting an error — which then hides every downstream check
// below that depends on spec.Capabilities, since it stays nil.
func decodeSecurityContextStrict(jsonRaw []byte) (corev1.SecurityContext, error) {
	var spec corev1.SecurityContext
	dec := json.NewDecoder(bytes.NewReader(jsonRaw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&spec)
	return spec, err
}

// validateSecurityContexts decodes Blueprint.SecurityContext and Podman.SecurityContext
// into corev1.SecurityContext to catch structural errors early, before any Kubernetes API call is made.
// It ensures the resulting security context is compatible with k8shelld's requirements.
func validateSecurityContexts(bp *models.Blueprint) []error {
	var errs []error

	if len(bp.SecurityContext) > 0 {
		jsonRaw, err := json.Marshal(bp.SecurityContext)
		if err != nil {
			errs = append(errs, newFieldError("securityContext", "securityContext: failed to marshal: %v", err))
		} else {
			spec, err := decodeSecurityContextStrict(jsonRaw)
			if err != nil {
				errs = append(errs, newFieldError("securityContext", "securityContext: invalid: %v", err))
			} else {
				if spec.RunAsUser != nil && *spec.RunAsUser != 0 {
					errs = append(errs, newFieldError("securityContext.runAsUser", "securityContext: runAsUser must be 0, got %d", *spec.RunAsUser))
				}
				if spec.RunAsGroup != nil && *spec.RunAsGroup != 0 {
					errs = append(errs, newFieldError("securityContext.runAsGroup", "securityContext: runAsGroup must be 0, got %d", *spec.RunAsGroup))
				}

				if spec.RunAsNonRoot != nil && *spec.RunAsNonRoot {
					errs = append(errs, newFieldError("securityContext.runAsNonRoot", "securityContext: runAsNonRoot cannot be true"))
				}
				if spec.ReadOnlyRootFilesystem != nil && *spec.ReadOnlyRootFilesystem {
					errs = append(errs, newFieldError("securityContext.readOnlyRootFilesystem", "securityContext: readOnlyRootFilesystem cannot be true"))
				}
				if spec.AllowPrivilegeEscalation != nil && !*spec.AllowPrivilegeEscalation {
					errs = append(errs, newFieldError("securityContext.allowPrivilegeEscalation", "securityContext: allowPrivilegeEscalation cannot be false"))
				}

				if spec.Capabilities != nil {
					errs = append(errs, validateCapabilityNames("securityContext.capabilities", spec.Capabilities)...)

					droppedAll := false
					for _, cap := range spec.Capabilities.Drop {
						if cap == "ALL" {
							droppedAll = true
							break
						}
					}

					if droppedAll {
						addedCaps := make(map[corev1.Capability]bool)
						for _, cap := range spec.Capabilities.Add {
							addedCaps[cap] = true
						}

						for _, reqCap := range requiredCaps {
							if !addedCaps[reqCap] {
								errs = append(errs, newFieldError("securityContext.capabilities",
									"securityContext: %s capability is required by k8shelld but dropped with ALL", reqCap))
							}
						}
					} else {
						for _, cap := range spec.Capabilities.Drop {
							for _, reqCap := range requiredCaps {
								if cap == reqCap {
									errs = append(errs, newFieldError("securityContext.capabilities",
										"securityContext: cannot drop %s capability", cap))
								}
							}
						}
					}
				}
			}
		}
	}

	if len(bp.Podman.SecurityContext) > 0 {
		jsonRaw, err := json.Marshal(bp.Podman.SecurityContext)
		if err != nil {
			errs = append(errs, newFieldError("podman.securityContext", "podman.securityContext: failed to marshal: %v", err))
		} else if spec, err := decodeSecurityContextStrict(jsonRaw); err != nil {
			errs = append(errs, newFieldError("podman.securityContext", "podman.securityContext: invalid: %v", err))
		} else {
			errs = append(errs, validateCapabilityNames("podman.securityContext.capabilities", spec.Capabilities)...)
		}
	}

	return errs
}

// NormalizeDNSLabel normalizes a string to be a valid DNS label / Helm release name:
// lowercase alphanumeric and hyphens, must start and end with alphanumeric, max 53 chars.
func NormalizeDNSLabel(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)
	reg := regexp.MustCompile(`[^a-z0-9-]+`)
	s = reg.ReplaceAllString(s, "-")
	reg = regexp.MustCompile(`^[^a-z0-9]+`)
	s = reg.ReplaceAllString(s, "")
	reg = regexp.MustCompile(`[^a-z0-9]+$`)
	s = reg.ReplaceAllString(s, "")
	reg = regexp.MustCompile(`-+`)
	s = reg.ReplaceAllString(s, "-")
	if len(s) > 53 {
		s = s[:53]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// GetBlueprint evaluates CEL expressions for a specific blueprint with given scope.
func (bm *BlueprintManager) GetBlueprint(name string, scope *BlueprintScope) (*models.Blueprint, error) {
	if scope == nil {
		return nil, fmt.Errorf("scope cannot be nil")
	}

	var org string
	if scope.User != nil {
		org = scope.User.Organization
	}

	rawBp, ok, err := bm.lookupOrgFromStore(org, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		bm.mu.RLock()
		fileBp, exists := bm.rawBlueprints[name]
		bm.mu.RUnlock()
		if !exists || fileBp.Org != "" {
			return nil, fmt.Errorf("blueprint %s not found: %w", name, ErrBlueprintNotFound)
		}
		rawBp = fileBp
	}

	return bm.evaluateRawBlueprint(rawBp, name, scope)
}

// evaluateRawBlueprint evaluates rawBp's CEL template against scope and
// decodes the result into a models.Blueprint. name is used only for error
// messages and the "blueprint" CEL variable.
func (bm *BlueprintManager) evaluateRawBlueprint(rawBp *RawBlueprint, name string, scope *BlueprintScope) (*models.Blueprint, error) {
	scope.Metadata.Name = NormalizeDNSLabel(rawBp.Name)
	var tmpl yamlcel.CELTemplate
	if err := rawBp.Node.Decode(&tmpl); err != nil {
		return nil, fmt.Errorf("failed to decode CEL template for %s: %w", name, err)
	}

	mapScope, err := scope.ToMap()
	mapScope["blueprint"] = name
	if err != nil {
		return nil, fmt.Errorf("failed to convert scope to map: %w", err)
	}

	docBytes, err := tmpl.EvalToBytes(mapScope, map[string]string{})
	if err != nil {
		return nil, fmt.Errorf("error evaluating CEL template for %s: %w", name, err)
	}

	var bp models.Blueprint
	decoder := yaml.NewDecoder(bytes.NewReader(docBytes))
	decoder.KnownFields(bm.knownFields)
	if err := decoder.Decode(&bp); err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	if bp.Name != "" {
		bp.Name = NormalizeDNSLabel(bp.Name)
	}

	bp.Metadata = *scope.Metadata

	return &bp, nil
}

// GetBlueprintChain returns the inheritance chain for the given blueprint name.
// The chain is an ordered slice from the root ancestor to the blueprint itself, e.g. ["base", "git-dev", "dev"].
// Returns nil if the blueprint is not found.
func (bm *BlueprintManager) GetBlueprintChain(name string) []string {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	rawBp, exists := bm.rawBlueprints[name]
	if !exists {
		return nil
	}
	return rawBp.InheritanceChain
}

// GetBlueprintTemplate returns the name of the immediate parent Template for
// the given blueprint name, or "" if it does not inherit from one. Returns
// ErrBlueprintNotFound if name is not a registered blueprint.
func (bm *BlueprintManager) GetBlueprintTemplate(name string) (string, error) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	rawBp, exists := bm.rawBlueprints[name]
	if !exists {
		return "", fmt.Errorf("blueprint %s not found: %w", name, ErrBlueprintNotFound)
	}
	return rawBp.Template, nil
}

// GetBlueprintsSummary returns a summary of every registered blueprint
// without evaluating CEL expressions: the file-based, global blueprints from
// the in-memory cache plus every org-scoped database blueprint read fresh
// from the backing store, so a row created or deleted out of band shows up
// immediately. Org names the organization an org-scoped blueprint belongs to
// (empty for a global one), IsGlobal is its inverse, and Template names the
// immediate parent template, if any.
func (bm *BlueprintManager) GetBlueprintsSummary() ([]*models.BlueprintSummary, error) {
	bm.mu.RLock()
	summaries := make([]*models.BlueprintSummary, 0, len(bm.rawBlueprints))
	for _, bp := range bm.rawBlueprints {
		if bp.Org != "" {
			continue // org blueprints are served from the store, below
		}
		summaries = append(summaries, &models.BlueprintSummary{
			Name:        bp.Name,
			Description: bp.Description,
			IsTemplate:  bp.IsTemplate,
			IsGlobal:    true,
			Template:    bp.Template,
			CreatedAt:   bp.CreatedAt,
			UpdatedAt:   bp.UpdatedAt,
		})
	}
	bm.mu.RUnlock()

	if bm.orgStore == nil {
		return summaries, nil
	}

	orgBlueprints, err := bm.orgStore.ListAllBlueprints()
	if err != nil {
		return nil, fmt.Errorf("list org blueprints from store: %w", err)
	}
	for _, ob := range orgBlueprints {
		_, _, template, _, _ := ParseBlueprintMeta(ob.YAML)
		summaries = append(summaries, &models.BlueprintSummary{
			Name:        ob.Name,
			Description: ob.Description,
			IsTemplate:  ob.IsTemplate,
			Org:         ob.Org,
			Template:    template,
			CreatedAt:   ob.CreatedAt,
			UpdatedAt:   ob.UpdatedAt,
		})
	}
	return summaries, nil
}

// GetRawBlueprint returns the raw (unevaluated) YAML content of the named
// blueprint, fully merged with any inherited Template content. CEL
// expressions are returned with a "!cel:" prefix so callers can display the
// template source without triggering evaluation.
func (bm *BlueprintManager) GetRawBlueprint(name string) (interface{}, error) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	rawBp, exists := bm.rawBlueprints[name]
	if !exists {
		return nil, fmt.Errorf("blueprint %s not found: %w", name, ErrBlueprintNotFound)
	}

	return bm.decodeRawNode(rawBp.Node)
}

// GetRawBlueprintOwn returns the raw (unevaluated) YAML content defined
// directly on the named blueprint, excluding any content inherited from its
// Template. A field present in GetRawBlueprint's output but absent here is
// inherited rather than set on this blueprint. CEL expressions are returned
// with a "!cel:" prefix, as in GetRawBlueprint.
func (bm *BlueprintManager) GetRawBlueprintOwn(name string) (interface{}, error) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	rawBp, exists := bm.rawBlueprints[name]
	if !exists {
		return nil, fmt.Errorf("blueprint %s not found: %w", name, ErrBlueprintNotFound)
	}

	return bm.decodeRawNode(rawBp.OwnNode)
}

// GetRawBlueprintScoped resolves name the same way GetRawBlueprint does, but
// when org is set it serves the org-scoped blueprint straight from the
// backing store (so a row changed or deleted out of band is reflected
// immediately), falling back to the file-based/global blueprint when org is
// empty or has no such row. It returns the fully merged content, the content
// defined directly on the blueprint (own), and the name of the immediate
// parent template, if any. CEL expressions are returned with a "!cel:"
// prefix, as in GetRawBlueprint.
func (bm *BlueprintManager) GetRawBlueprintScoped(org, name string) (merged, own interface{}, template string, err error) {
	rawBp, ok, err := bm.lookupOrgFromStore(org, name)
	if err != nil {
		return nil, nil, "", err
	}
	if !ok {
		bm.mu.RLock()
		fileBp, exists := bm.rawBlueprints[name]
		bm.mu.RUnlock()
		if !exists || fileBp.Org != "" {
			return nil, nil, "", fmt.Errorf("blueprint %s not found: %w", name, ErrBlueprintNotFound)
		}
		rawBp = fileBp
	}

	if merged, err = bm.decodeRawNode(rawBp.Node); err != nil {
		return nil, nil, "", err
	}
	if own, err = bm.decodeRawNode(rawBp.OwnNode); err != nil {
		return nil, nil, "", err
	}
	return merged, own, rawBp.Template, nil
}

// decodeRawNode clones node (preserving CEL expressions as "!cel:"-prefixed
// strings) and decodes it into a plain interface{} tree.
func (bm *BlueprintManager) decodeRawNode(node *yaml.Node) (interface{}, error) {
	clonedNode := bm.cloneAndProcessCELNodes(node)

	var temp interface{}
	if err := clonedNode.Decode(&temp); err != nil {
		return nil, fmt.Errorf("failed to decode raw blueprint: %w", err)
	}

	return temp, nil
}

// ListBlueprintNames returns all available file-based, global blueprint
// names. Org-scoped database blueprints are excluded (see GetBlueprintsSummary).
func (bm *BlueprintManager) ListBlueprintNames() []string {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	names := make([]string, 0, len(bm.rawBlueprints))
	for name, bp := range bm.rawBlueprints {
		if bp.Org != "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// HasGlobalBlueprint reports whether a file-based, global blueprint (template
// or not) named name is currently registered. Org-scoped database blueprints
// are not considered. Used to stop an org blueprint from being created under
// a name that a global blueprint already owns, which it would otherwise
// silently shadow for that org.
func (bm *BlueprintManager) HasGlobalBlueprint(name string) bool {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	bp, ok := bm.rawBlueprints[name]
	return ok && bp.Org == ""
}

// GetDefaultUserBlueprint returns the name of the first non-template blueprint
// (in alphabetical order) that the user is authorised to use. It is used when
// no blueprint is specified explicitly in the userstr.
func (bm *BlueprintManager) GetDefaultUserBlueprint(user *models.User) (string, error) {
	if user == nil {
		return "", fmt.Errorf("user cannot be nil")
	}

	if len(user.Blueprints) == 0 {
		return "", fmt.Errorf("no blueprints defined for user %s", user.Username)
	}

	bm.mu.RLock()
	defer bm.mu.RUnlock()

	blueprintNames := make([]string, 0, len(bm.rawBlueprints))
	for name := range bm.rawBlueprints {
		blueprintNames = append(blueprintNames, name)
	}
	sort.Strings(blueprintNames)

	for _, bp := range blueprintNames {
		if user.HasBlueprint(bp) && !bm.rawBlueprints[bp].IsTemplate {
			return bp, nil
		}
	}

	return "", fmt.Errorf("no accessible blueprints found for user %s", user.Username)
}

// GetAllBlueprints evaluates all blueprints with the given scope.
func (bm *BlueprintManager) GetAllBlueprints(scope *BlueprintScope) (map[string]*models.Blueprint, error) {
	bm.mu.RLock()
	blueprintNames := make([]string, 0, len(bm.rawBlueprints))
	for name := range bm.rawBlueprints {
		blueprintNames = append(blueprintNames, name)
	}
	bm.mu.RUnlock()

	result := make(map[string]*models.Blueprint)
	for _, name := range blueprintNames {
		bp, err := bm.GetBlueprint(name, scope)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate blueprint %s: %w", name, err)
		}
		result[name] = bp
	}

	return result, nil
}

// loadRawBlueprints loads raw blueprints from YAML files.
func (bm *BlueprintManager) loadRawBlueprints(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip Kubernetes ConfigMap internal directories (e.g. ..2024_01_01_12_00_00.000000000)
		// which contain the real files that are symlinked from the mount root.
		// Walking both would cause duplicate blueprint names.
		if strings.HasPrefix(d.Name(), "..") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !isYAMLFile(path) {
			return nil
		}

		root, err := bm.processor.LoadFile(path)
		if err != nil {
			return fmt.Errorf("failed to load YAML file '%s': %w", path, err)
		}

		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("failed to resolve path '%s': %w", path, err)
		}
		if err := processIncludes(root, filepath.Dir(absPath), []string{absPath}); err != nil {
			return fmt.Errorf("failed to process !include in '%s': %w", path, err)
		}

		if err := bm.extractRawBlueprints(root, path); err != nil {
			return err
		}

		// Stamp every blueprint that came from this file with the file's
		// last-modified time. A file carries no separate creation record, so
		// CreatedAt and UpdatedAt are deliberately the same value.
		if info, ierr := d.Info(); ierr == nil {
			mt := info.ModTime()
			for _, bp := range bm.rawBlueprints {
				if bp.SourceFile == path && bp.CreatedAt.IsZero() {
					bp.CreatedAt = mt
					bp.UpdatedAt = mt
				}
			}
		}
		return nil
	})
}

// extractRawBlueprints extracts raw blueprints from YAML nodes.
func (bm *BlueprintManager) extractRawBlueprints(root *yaml.Node, path string) error {
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		return bm.extractRawBlueprints(root.Content[0], path)
	}

	switch root.Kind {
	case yaml.MappingNode:
		return bm.extractFromMapping(root, path)
	case yaml.SequenceNode:
		return bm.extractFromSequence(root, path)
	default:
		return fmt.Errorf("unsupported YAML structure at %s: expected mapping or sequence, got %v", path, root.Kind)
	}
}

// extractFromMapping extracts blueprints from a mapping node.
func (bm *BlueprintManager) extractFromMapping(root *yaml.Node, path string) error {
	var data map[string]interface{}
	if err := root.Decode(&data); err != nil {
		return fmt.Errorf("failed to decode YAML mapping at %s: %w", path, err)
	}

	switch {
	case data["blueprint"] != nil:
		blueprintNode := bm.findChildNode(root, "blueprint")
		if blueprintNode == nil {
			return fmt.Errorf("blueprint key found but node not accessible at %s", path)
		}
		return bm.extractSingleRawBlueprint(blueprintNode, path)
	case data["blueprints"] != nil:
		blueprintsNode := bm.findChildNode(root, "blueprints")
		if blueprintsNode == nil {
			return fmt.Errorf("blueprints key found but node not accessible at %s", path)
		}
		return bm.extractMultipleRawBlueprints(blueprintsNode, path)
	default:
		return bm.extractSingleRawBlueprint(root, path)
	}
}

// findChildNode finds a child node by key in a mapping node.
func (bm *BlueprintManager) findChildNode(parent *yaml.Node, key string) *yaml.Node {
	if parent.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i < len(parent.Content); i += 2 {
		if i+1 < len(parent.Content) && parent.Content[i].Value == key {
			return parent.Content[i+1]
		}
	}
	return nil
}

// extractFromSequence extracts blueprints from a sequence node.
func (bm *BlueprintManager) extractFromSequence(root *yaml.Node, path string) error {
	return bm.extractMultipleRawBlueprints(root, path)
}

// extractSingleRawBlueprint extracts a single raw blueprint.
func (bm *BlueprintManager) extractSingleRawBlueprint(node *yaml.Node, path string) error {
	var bpData map[string]interface{}
	if err := node.Decode(&bpData); err != nil {
		bpData = make(map[string]interface{})
	}

	name := generateRandomName("bp")
	if n, ok := bpData["name"].(string); ok {
		name = n
	}

	descr, _ := bpData["description"].(string)
	descr = strings.Join(strings.Fields(descr), " ")

	template := ""
	if t, ok := bpData["template"].(string); ok {
		template = t
	}

	isTemplate := false
	if t, ok := bpData["isTemplate"].(bool); ok {
		isTemplate = t
	}

	if existing, exists := bm.rawBlueprints[name]; exists {
		return fmt.Errorf("duplicate blueprint name %q: already defined in %s", name, existing.SourceFile)
	}

	bm.rawBlueprints[name] = &RawBlueprint{
		Name:        name,
		Description: descr,
		Template:    template,
		IsTemplate:  isTemplate,
		SourceFile:  path,
		Node:        node,
	}

	return nil
}

// extractMultipleRawBlueprints extracts multiple raw blueprints from a list.
func (bm *BlueprintManager) extractMultipleRawBlueprints(node *yaml.Node, path string) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("expected sequence node for blueprints at %s, got %v", path, node.Kind)
	}

	for _, childNode := range node.Content {
		var item map[string]interface{}
		if err := childNode.Decode(&item); err != nil {
			item = make(map[string]interface{})
		}

		name := generateRandomName("bp")
		if n, ok := item["name"].(string); ok {
			name = n
		}

		template := ""
		if t, ok := item["template"].(string); ok {
			template = t
		}

		isTemplate := false
		if t, ok := item["isTemplate"].(bool); ok {
			isTemplate = t
		}

		descr, _ := item["description"].(string)
		descr = strings.Join(strings.Fields(descr), " ")

		if existing, exists := bm.rawBlueprints[name]; exists {
			return fmt.Errorf("duplicate blueprint name %q: already defined in %s", name, existing.SourceFile)
		}

		bm.rawBlueprints[name] = &RawBlueprint{
			Name:        name,
			Description: descr,
			Template:    template,
			IsTemplate:  isTemplate,
			SourceFile:  path,
			Node:        childNode,
		}
	}

	return nil
}

// cloneAndProcessCELNodes recursively clones YAML nodes and adds cel:: prefix to !!cel tagged values
func (bm *BlueprintManager) cloneAndProcessCELNodes(node *yaml.Node) *yaml.Node {
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

	if node.Tag == "!cel" {
		if node.Kind == yaml.ScalarNode {
			cloned.Tag = "!!str"
			cloned.Value = "!cel:" + node.Value
		}
	}

	if len(node.Content) > 0 {
		cloned.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			cloned.Content[i] = bm.cloneAndProcessCELNodes(child)
		}
	}

	return cloned
}

//** helper functions **//

// processIncludes walks a YAML node tree and resolves !include tags by loading
// and inlining the referenced YAML files in-place. This happens before CEL
// evaluation, so included fragments may themselves contain CEL expressions.
// baseDir is used to resolve relative paths. stack holds the absolute paths of
// files currently on the include chain and is used for cycle detection.
func processIncludes(node *yaml.Node, baseDir string, stack []string) error {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := processIncludes(child, baseDir, stack); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		// Content is [key0, val0, key1, val1, ...]; only value nodes (odd indices) are candidates
		for i := 1; i < len(node.Content); i += 2 {
			val := node.Content[i]
			if val.Kind == yaml.ScalarNode && val.Tag == "!include" {
				if err := resolveInclude(val, baseDir, stack); err != nil {
					return err
				}
			} else {
				if err := processIncludes(val, baseDir, stack); err != nil {
					return err
				}
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if child.Kind == yaml.ScalarNode && child.Tag == "!include" {
				if err := resolveInclude(child, baseDir, stack); err != nil {
					return err
				}
			} else {
				if err := processIncludes(child, baseDir, stack); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// resolveInclude replaces a !include scalar node in-place with the parsed YAML
// content of the referenced file. Nested !include tags within the included file
// are resolved recursively using the included file's directory as the base.
func resolveInclude(node *yaml.Node, baseDir string, stack []string) error {
	includePath := node.Value
	if !filepath.IsAbs(includePath) {
		includePath = filepath.Join(baseDir, includePath)
	}
	includePath = filepath.Clean(includePath)

	for _, s := range stack {
		if s == includePath {
			return fmt.Errorf("!include cycle detected: %s -> %s", strings.Join(stack, " -> "), includePath)
		}
	}

	raw, err := os.ReadFile(includePath)
	if err != nil {
		return fmt.Errorf("!include '%s': %w", includePath, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("!include '%s': parse error: %w", includePath, err)
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("!include '%s': file is empty or invalid", includePath)
	}

	included := doc.Content[0] // unwrap the DocumentNode wrapper

	includeBaseDir := filepath.Dir(includePath)
	newStack := append(append([]string{}, stack...), includePath)
	if err := processIncludes(included, includeBaseDir, newStack); err != nil {
		return fmt.Errorf("!include '%s': %w", includePath, err)
	}

	node.Kind = included.Kind
	node.Tag = included.Tag
	node.Value = included.Value
	node.Content = included.Content
	node.Style = included.Style

	return nil
}

// generateRandomName creates a random name with the given prefix.
func generateRandomName(prefix string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return prefix + "-xxxx"
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b))
}

// isYAMLFile checks if a file has a YAML extension
func isYAMLFile(filename string) bool {
	ext := filepath.Ext(filename)
	return ext == ".yaml" || ext == ".yml"
}
