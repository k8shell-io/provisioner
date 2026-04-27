package blueprint

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
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
)

// RawBlueprint represents an unprocessed blueprint with CEL expressions intact.
type RawBlueprint struct {
	Name             string
	Description      string
	Template         string
	IsTemplate       bool
	SourceFile       string
	Node             *yaml.Node
	InheritanceChain []string // ordered list of blueprint names from root ancestor to this blueprint
}

type BlueprintScope struct {
	User          *models.User              `yaml:"user"`
	WorkspaceName string                    `yaml:"workspaceName"`
	Metadata      *models.BlueprintMetadata `yaml:"metadata"`
}

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
}

// BlueprintManager manages blueprints with lazy CEL evaluation.
type BlueprintManager struct {
	log           *zerolog.Logger          // Logger for the blueprint manager
	rawBlueprints map[string]*RawBlueprint // Map of blueprint names to their raw definitions
	knownFields   bool                     // Whether to allow unknown fields in YAML decoding
	strategies    MergeStrategies          // Custom strategies for merging lists in blueprints
	processor     *config.Processor        // YAML processor for parsing and validating blueprints
	watcher       *Watcher                 // the file watcher
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
			Auths:      []string{models.AuthMethodPublicKey, models.AuthMethodPassword},
			AuthKeys:   []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC3..."},
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
		mu: sync.RWMutex{},
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
	blueprintNames := make([]string, 0, len(bm.rawBlueprints))
	for name := range bm.rawBlueprints {
		blueprintNames = append(blueprintNames, name)
	}
	bm.mu.RUnlock()

	for _, name := range blueprintNames {
		bp, err := bm.GetBlueprint(name, validationScope)
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
		for _, e := range validateSecurityContexts(bp) {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %w", name, e))
		}
	}

	return allErrors
}

// validateClaimSpecs decodes each storage claimSpec into corev1.PersistentVolumeClaimSpec
// to catch structural errors early, before any Kubernetes API call is made.
func validateClaimSpecs(bp *models.Blueprint) []error {
	type namedStorage struct {
		name    string
		storage models.Storage
	}

	var all []namedStorage
	for name, s := range bp.Storages {
		all = append(all, namedStorage{name, s})
	}
	for name, s := range bp.Podman.Storages {
		all = append(all, namedStorage{"podman." + name, s})
	}

	var errs []error
	for _, ns := range all {
		if ns.storage.ClaimSpec == nil {
			continue
		}
		jsonRaw, err := json.Marshal(ns.storage.ClaimSpec)
		if err != nil {
			errs = append(errs, fmt.Errorf("storage %q: failed to marshal claimSpec: %w", ns.name, err))
			continue
		}
		var spec corev1.PersistentVolumeClaimSpec
		if err := json.Unmarshal(jsonRaw, &spec); err != nil {
			errs = append(errs, fmt.Errorf("storage %q: invalid claimSpec: %w", ns.name, err))
		}
	}
	return errs
}

// validateSecurityContexts decodes Blueprint.SecurityContext and Podman.SecurityContext
// into corev1.SecurityContext to catch structural errors early, before any Kubernetes API call is made.
// It ensures the resulting security context is compatible with k8shelld's requirements.
func validateSecurityContexts(bp *models.Blueprint) []error {
	var errs []error

	if len(bp.SecurityContext) > 0 {
		jsonRaw, err := json.Marshal(bp.SecurityContext)
		if err != nil {
			errs = append(errs, fmt.Errorf("securityContext: failed to marshal: %w", err))
		} else {
			var spec corev1.SecurityContext
			if err := json.Unmarshal(jsonRaw, &spec); err != nil {
				errs = append(errs, fmt.Errorf("securityContext: invalid: %w", err))
			} else {
				if spec.RunAsUser != nil && *spec.RunAsUser != 0 {
					errs = append(errs, fmt.Errorf("securityContext: runAsUser must be 0, got %d", *spec.RunAsUser))
				}
				if spec.RunAsGroup != nil && *spec.RunAsGroup != 0 {
					errs = append(errs, fmt.Errorf("securityContext: runAsGroup must be 0, got %d", *spec.RunAsGroup))
				}

				if spec.RunAsNonRoot != nil && *spec.RunAsNonRoot {
					errs = append(errs, fmt.Errorf("securityContext: runAsNonRoot cannot be true"))
				}
				if spec.ReadOnlyRootFilesystem != nil && *spec.ReadOnlyRootFilesystem {
					errs = append(errs, fmt.Errorf("securityContext: readOnlyRootFilesystem cannot be true"))
				}
				if spec.AllowPrivilegeEscalation != nil && !*spec.AllowPrivilegeEscalation {
					errs = append(errs, fmt.Errorf("securityContext: allowPrivilegeEscalation cannot be false"))
				}

				if spec.Capabilities != nil {
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
								errs = append(errs,
									fmt.Errorf("securityContext: %s capability is required by k8shelld but dropped with ALL", reqCap))
							}
						}
					} else {
						for _, cap := range spec.Capabilities.Drop {
							for _, reqCap := range requiredCaps {
								if cap == reqCap {
									errs = append(errs,
										fmt.Errorf("securityContext: cannot drop %s capability", cap))
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
			errs = append(errs, fmt.Errorf("podman.securityContext: failed to marshal: %w", err))
		} else {
			var spec corev1.SecurityContext
			if err := json.Unmarshal(jsonRaw, &spec); err != nil {
				errs = append(errs, fmt.Errorf("podman.securityContext: invalid: %w", err))
			}
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

	bm.mu.RLock()
	rawBp, exists := bm.rawBlueprints[name]
	bm.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("blueprint %s not found: %w", name, ErrBlueprintNotFound)
	}

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

// GetBlueprintsSummary returns a summary of all available blueprints without evaluating CEL expressions.
func (bm *BlueprintManager) GetBlueprintsSummary() []*models.BlueprintSummary {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	summaries := make([]*models.BlueprintSummary, 0, len(bm.rawBlueprints))
	for name, bp := range bm.rawBlueprints {
		summaries = append(summaries, &models.BlueprintSummary{
			Name:        name,
			Description: bp.Description,
			IsTemplate:  bp.IsTemplate,
		})
	}
	return summaries
}

func (bm *BlueprintManager) GetRawBlueprint(name string) (interface{}, error) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	rawBp, exists := bm.rawBlueprints[name]
	if !exists {
		return nil, fmt.Errorf("blueprint %s not found: %w", name, ErrBlueprintNotFound)
	}

	clonedNode := bm.cloneAndProcessCELNodes(rawBp.Node)

	var temp interface{}
	if err := clonedNode.Decode(&temp); err != nil {
		return nil, fmt.Errorf("failed to decode raw blueprint: %w", err)
	}

	return temp, nil
}

// ListBlueprintNames returns all available blueprint names.
func (bm *BlueprintManager) ListBlueprintNames() []string {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	names := make([]string, 0, len(bm.rawBlueprints))
	for name := range bm.rawBlueprints {
		names = append(names, name)
	}
	return names
}

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

		return bm.extractRawBlueprints(root, path)
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
