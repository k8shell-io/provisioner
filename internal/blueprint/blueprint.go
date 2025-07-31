package blueprint

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	identity "github.com/k8shell-io/identity/pkg/models"
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/k8shell-io/provisioner/pkg/models"
	"github.com/k8shell-io/yaml-cel/pkg/yamlcel"
	"github.com/k8shell-io/yaml-config/pkg/yamlconfig"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// RawBlueprint represents an unprocessed blueprint with CEL expressions intact.
type RawBlueprint struct {
	Name     string
	Template string
	Node     *yaml.Node
}

type BlueprintScope struct {
	Blueprint string        `yaml:"blueprint"`
	User      identity.User `yaml:"user"`
	Repo      models.Repo   `yaml:"repo"`
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
	log           *zerolog.Logger
	rawBlueprints map[string]*RawBlueprint
	knownFields   bool
	strategies    MergeStrategies
	processor     *yamlconfig.Processor
	watcher       *fsnotify.Watcher
	watchDir      string
	watchEnabled  bool
	mu            sync.RWMutex
	reloadTimer   *time.Timer
	reloadDelay   time.Duration
	stopChan      chan struct{}
}

// TestScope creates a minimal BlueprintScope for testing purposes.
func TestScope() *BlueprintScope {
	return &BlueprintScope{
		Blueprint: "testblueprint",
		User: identity.User{
			Username:     "testuser",
			IsValid:      true,
			ExpiresAt:    time.Now().Add(24 * time.Hour),
			UID:          1000,
			GID:          1000,
			Fullname:     "Test User",
			AccessToken:  "testtoken",
			Email:        "testuser@example.com",
			Password:     "testpassword",
			Auths:        []identity.AuthMethod{identity.AuthMethodPublicKey, identity.AuthMethodPassword},
			AuthKeys:     []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC3..."},
			Locked:       false,
			FailedLogins: 0,
			Channels:     []identity.Channel{identity.ChannelShell, identity.ChannelSFTP},
			Envs:         []string{},
			Roles:        []identity.Role{"role1", "role2"},
			Blueprints:   []string{"testblueprint"},
			Source:       "testsource",
		},
		Repo: models.Repo{
			Owner: "testowner",
			Name:  "testrepo",
		},
	}
}

// NewBlueprintManager creates a new blueprint manager.
func NewBlueprintManager(opts LoadOptions) (*BlueprintManager, error) {
	if opts.Strategies == nil {
		opts.Strategies = MergeStrategies{}
	}

	manager := &BlueprintManager{
		log:           log.NewLogger("blueprint"),
		rawBlueprints: make(map[string]*RawBlueprint),
		knownFields:   true,
		strategies:    opts.Strategies,
		processor:     yamlconfig.NewProcessor(yamlconfig.DefaultOptions()),
		watchDir:      opts.Dir,
		watchEnabled:  opts.EnableWatch,
		reloadDelay:   500 * time.Millisecond,
		stopChan:      make(chan struct{}),
	}

	if err := manager.loadAndValidateBlueprints(); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	if opts.EnableWatch {
		if err := manager.setupWatcher(); err != nil {
			return nil, fmt.Errorf("failed to setup file watcher: %w", err)
		}
	}

	manager.log.Info().Msgf("Loaded %d blueprints from %s, watch enabled: %v", len(manager.rawBlueprints),
		opts.Dir, manager.watchEnabled)
	return manager, nil
}

// loadAndValidateBlueprints loads and validates all blueprints atomically
func (bm *BlueprintManager) loadAndValidateBlueprints() error {
	tempBlueprints := make(map[string]*RawBlueprint)

	bm.mu.Lock()
	originalBlueprints := bm.rawBlueprints
	bm.rawBlueprints = tempBlueprints
	bm.mu.Unlock()

	if err := bm.loadRawBlueprints(bm.watchDir); err != nil {
		bm.mu.Lock()
		bm.rawBlueprints = originalBlueprints
		bm.mu.Unlock()
		return fmt.Errorf("failed to load blueprints: %w", err)
	}

	if err := bm.resolveInheritance(); err != nil {
		bm.mu.Lock()
		bm.rawBlueprints = originalBlueprints
		bm.mu.Unlock()
		return fmt.Errorf("failed to resolve inheritance: %w", err)
	}

	if errs := bm.validateAllBlueprints(); len(errs) > 0 {
		bm.mu.Lock()
		bm.rawBlueprints = originalBlueprints
		bm.mu.Unlock()
		out := ""
		for _, err := range errs {
			out += fmt.Sprintf("%s\n", err.Error())
		}
		return fmt.Errorf("failed to validate blueprint:\n%s", out)
	}

	bm.mu.Lock()
	bm.rawBlueprints = tempBlueprints
	bm.mu.Unlock()

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
		}
		v := bp.Validate()
		if v != nil {
			allErrors = append(allErrors, fmt.Errorf("blueprint '%s': %v", name, v))
		}
	}

	return allErrors
}

// setupWatcher initializes the file system watcher
func (bm *BlueprintManager) setupWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	bm.watcher = watcher

	err = filepath.WalkDir(bm.watchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return bm.watcher.Add(path)
		}
		return nil
	})

	if err != nil {
		bm.watcher.Close()
		return err
	}

	go bm.watchLoop()

	return nil
}

// watchLoop handles file system events
func (bm *BlueprintManager) watchLoop() {
	for {
		select {
		case event, ok := <-bm.watcher.Events:
			if !ok {
				return
			}

			if !isYAMLFile(event.Name) {
				continue
			}

			switch {
			case event.Op&fsnotify.Write == fsnotify.Write,
				event.Op&fsnotify.Create == fsnotify.Create,
				event.Op&fsnotify.Remove == fsnotify.Remove,
				event.Op&fsnotify.Rename == fsnotify.Rename:
				bm.scheduleReload()
			}

		case err, ok := <-bm.watcher.Errors:
			if !ok {
				return
			}
			fmt.Printf("Watcher error: %v\n", err)

		case <-bm.stopChan:
			return
		}
	}
}

// scheduleReload debounces multiple file changes and schedules a reload
func (bm *BlueprintManager) scheduleReload() {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if bm.reloadTimer != nil {
		bm.reloadTimer.Stop()
	}

	bm.reloadTimer = time.AfterFunc(bm.reloadDelay, func() {
		bm.log.Info().Msg("Reloading blueprints due to file changes")
		if err := bm.loadAndValidateBlueprints(); err != nil {
			bm.log.Error().Err(err).Msg("Failed to reload blueprints")
		} else {
			bm.log.Info().Msg("Blueprints reloaded successfully")
		}
	})
}

// isYAMLFile checks if a file has a YAML extension
func isYAMLFile(filename string) bool {
	ext := filepath.Ext(filename)
	return ext == ".yaml" || ext == ".yml"
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
		return nil, fmt.Errorf("blueprint %s not found", name)
	}

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

	return &bp, nil
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

// Close stops the file watcher and cleans up resources
func (bm *BlueprintManager) Close() error {
	if !bm.watchEnabled {
		return nil
	}

	close(bm.stopChan)

	bm.mu.Lock()
	if bm.reloadTimer != nil {
		bm.reloadTimer.Stop()
	}
	bm.mu.Unlock()

	if bm.watcher != nil {
		return bm.watcher.Close()
	}

	bm.log.Info().Msg("Blueprint manager closed")
	return nil
}

// loadRawBlueprints loads raw blueprints from YAML files.
func (bm *BlueprintManager) loadRawBlueprints(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
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

	template := ""
	if t, ok := bpData["template"].(string); ok {
		template = t
	}

	bm.rawBlueprints[name] = &RawBlueprint{
		Name:     name,
		Template: template,
		Node:     node,
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

		bm.rawBlueprints[name] = &RawBlueprint{
			Name:     name,
			Template: template,
			Node:     childNode,
		}
	}

	return nil
}

// resolveInheritance resolves template inheritance at YAML level.
func (bm *BlueprintManager) resolveInheritance() error {
	resolved := make(map[string]*RawBlueprint)

	for name := range bm.rawBlueprints {
		if _, exists := resolved[name]; !exists {
			resolvedBp, err := bm.resolveRawTemplate(name, map[string]bool{})
			if err != nil {
				return err
			}
			resolved[name] = resolvedBp
		}
	}

	bm.rawBlueprints = resolved
	return nil
}

// resolveRawTemplate recursively resolves template inheritance at YAML level.
func (bm *BlueprintManager) resolveRawTemplate(bpName string, visited map[string]bool) (*RawBlueprint, error) {
	if visited[bpName] {
		return nil, fmt.Errorf("circular template reference: %s", bpName)
	}
	visited[bpName] = true

	bp, found := bm.rawBlueprints[bpName]
	if !found {
		return nil, fmt.Errorf("blueprint %s not found", bpName)
	}

	if bp.Template == "" {
		return bp, nil
	}

	parent, err := bm.resolveRawTemplate(bp.Template, visited)
	if err != nil {
		return nil, err
	}

	// Merge YAML nodes
	mergedNode, err := bm.mergeYAMLNodes(parent.Node, bp.Node)
	if err != nil {
		return nil, fmt.Errorf("failed to merge templates for %s: %w", bpName, err)
	}

	return &RawBlueprint{
		Name:     bp.Name,
		Template: "",
		Node:     mergedNode,
	}, nil
}

// mergeYAMLNodes merges two YAML nodes, preserving CEL expressions.
func (bm *BlueprintManager) mergeYAMLNodes(parent, child *yaml.Node) (*yaml.Node, error) {
	// Instead of decoding to maps (which loses tags), we need to merge at the node level
	return bm.mergeYAMLNodesWithTags(parent, child)
}

// mergeYAMLNodesWithTags merges YAML nodes while preserving all tags
func (bm *BlueprintManager) mergeYAMLNodesWithTags(parent, child *yaml.Node) (*yaml.Node, error) {
	if child.Kind != yaml.MappingNode {
		return child, nil
	}

	if parent.Kind != yaml.MappingNode {
		return child, nil
	}

	result := &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: make([]*yaml.Node, 0),
	}

	parentKeys := make(map[string]*yaml.Node)
	for i := 0; i < len(parent.Content); i += 2 {
		if i+1 < len(parent.Content) {
			key := parent.Content[i].Value
			value := parent.Content[i+1]
			parentKeys[key] = value
		}
	}

	processedKeys := make(map[string]bool)

	for i := 0; i < len(child.Content); i += 2 {
		if i+1 < len(child.Content) {
			keyNode := child.Content[i]
			valueNode := child.Content[i+1]
			key := keyNode.Value

			if parentValue, exists := parentKeys[key]; exists {
				mergedValue, err := bm.mergeValueNodes(parentValue, valueNode, key)
				if err != nil {
					return nil, err
				}
				result.Content = append(result.Content, keyNode, mergedValue)
			} else {
				result.Content = append(result.Content, keyNode, valueNode)
			}
			processedKeys[key] = true
		}
	}

	for i := 0; i < len(parent.Content); i += 2 {
		if i+1 < len(parent.Content) {
			keyNode := parent.Content[i]
			valueNode := parent.Content[i+1]
			key := keyNode.Value

			if !processedKeys[key] {
				result.Content = append(result.Content, keyNode, valueNode)
			}
		}
	}

	return result, nil
}

// mergeValueNodes merges two value nodes based on their types and merge strategies
func (bm *BlueprintManager) mergeValueNodes(parentValue, childValue *yaml.Node, key string) (*yaml.Node, error) {
	if parentValue.Kind == yaml.MappingNode && childValue.Kind == yaml.MappingNode {
		return bm.mergeYAMLNodesWithTags(parentValue, childValue)
	}

	if parentValue.Kind == yaml.SequenceNode && childValue.Kind == yaml.SequenceNode {
		return bm.mergeSequenceNodes(parentValue, childValue, key)
	}

	return childValue, nil
}

// mergeSequenceNodes merges two sequence nodes based on the configured strategy
func (bm *BlueprintManager) mergeSequenceNodes(parentSeq, childSeq *yaml.Node, key string) (*yaml.Node, error) {
	if strategy, exists := bm.strategies[key]; exists {
		var parentList, childList []interface{}

		if err := parentSeq.Decode(&parentList); err != nil {
			return nil, fmt.Errorf("failed to decode parent sequence for key %s: %w", key, err)
		}

		if err := childSeq.Decode(&childList); err != nil {
			return nil, fmt.Errorf("failed to decode child sequence for key %s: %w", key, err)
		}

		mergedList := strategy(parentList, childList)

		var resultNode yaml.Node
		if err := resultNode.Encode(mergedList); err != nil {
			return nil, fmt.Errorf("failed to encode merged sequence for key %s: %w", key, err)
		}

		return &resultNode, nil
	}

	// Default strategy: append child items to parent items
	result := &yaml.Node{
		Kind:    yaml.SequenceNode,
		Tag:     "!!seq",
		Content: make([]*yaml.Node, 0),
	}

	result.Content = append(result.Content, parentSeq.Content...)
	result.Content = append(result.Content, childSeq.Content...)

	return result, nil
}

// generateRandomName creates a random name with the given prefix.
func generateRandomName(prefix string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return prefix + "-xxxx"
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b))
}
