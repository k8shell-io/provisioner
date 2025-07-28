package blueprint

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/k8shell-io/yaml-cel/pkg/yamlcel"
	"github.com/k8shell-io/yaml-config/pkg/yamlconfig"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// Blueprint represents the loaded blueprint from YAML.
type Blueprint struct {
	Name     string `yaml:"name"`
	Template string `yaml:"template,omitempty"`
	Raw      map[string]interface{}
}

// RawBlueprint represents an unprocessed blueprint with CEL expressions intact.
type RawBlueprint struct {
	Name     string
	Template string
	Node     *yaml.Node // Raw YAML node preserving CEL expressions
}

// MergeStrategies allow custom list merging strategies per dotted path.
type MergeStrategies map[string]func(dst, src []interface{}) []interface{}

// LoadOptions contains configuration for loading blueprints.
type LoadOptions struct {
	Dir        string
	Strategies MergeStrategies
}

// BlueprintManager manages blueprints with lazy CEL evaluation.
type BlueprintManager struct {
	rawBlueprints map[string]*RawBlueprint
	strategies    MergeStrategies
	processor     *yamlconfig.Processor
}

// NewBlueprintManager creates a new blueprint manager.
func NewBlueprintManager(opts LoadOptions) (*BlueprintManager, error) {
	if opts.Strategies == nil {
		opts.Strategies = MergeStrategies{}
	}

	manager := &BlueprintManager{
		rawBlueprints: make(map[string]*RawBlueprint),
		strategies:    opts.Strategies,
		processor:     yamlconfig.NewProcessor(yamlconfig.DefaultOptions()),
	}

	if err := manager.loadRawBlueprints(opts.Dir); err != nil {
		return nil, err
	}

	if err := manager.resolveInheritance(); err != nil {
		return nil, err
	}

	return manager, nil
}

// loadRawBlueprints loads raw blueprints from YAML files WITHOUT any processing to preserve CEL tags.
func (bm *BlueprintManager) loadRawBlueprints(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml" {
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
	log.Info().Str("path", path).Msg("Root is sequence, treating as multiple blueprints")
	return bm.extractMultipleRawBlueprints(root, path)
}

// extractSingleRawBlueprint extracts a single raw blueprint.
func (bm *BlueprintManager) extractSingleRawBlueprint(node *yaml.Node, path string) error {
	var bpData map[string]interface{}
	if err := node.Decode(&bpData); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("Could not fully decode blueprint data, using defaults")
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

// mergeYAMLNodesWithTags merges YAML nodes while preserving all tags including !CEL
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

// GetBlueprint evaluates CEL expressions for a specific blueprint with given scope.
func (bm *BlueprintManager) GetBlueprint(name string, scope map[string]any) (*Blueprint, error) {
	if scope == nil {
		return nil, fmt.Errorf("scope cannot be nil")
	}

	rawBp, exists := bm.rawBlueprints[name]
	if !exists {
		return nil, fmt.Errorf("blueprint %s not found", name)
	}

	var tmpl yamlcel.CELTemplate
	if err := rawBp.Node.Decode(&tmpl); err != nil {
		return nil, fmt.Errorf("failed to decode CEL template for %s: %w", name, err)
	}

	doc, err := tmpl.Eval(scope, map[string]string{})
	if err != nil {
		return nil, fmt.Errorf("error evaluating CEL template for %s: %w", name, err)
	}

	var raw map[string]interface{}
	if err := doc.Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to decode evaluated result for %s: %w", name, err)
	}

	return &Blueprint{
		Name: name,
		Raw:  raw,
	}, nil
}

// ListBlueprintNames returns all available blueprint names.
func (bm *BlueprintManager) ListBlueprintNames() []string {
	names := make([]string, 0, len(bm.rawBlueprints))
	for name := range bm.rawBlueprints {
		names = append(names, name)
	}
	return names
}

// GetAllBlueprints evaluates all blueprints with the given scope.
func (bm *BlueprintManager) GetAllBlueprints(scope map[string]any) (map[string]*Blueprint, error) {
	result := make(map[string]*Blueprint)

	for name := range bm.rawBlueprints {
		bp, err := bm.GetBlueprint(name, scope)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate blueprint %s: %w", name, err)
		}
		result[name] = bp
	}

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
