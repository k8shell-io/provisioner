package blueprint

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

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
		return nil, fmt.Errorf("blueprint '%s' not found: %w", bpName, ErrBlueprintNotFound)
	}

	if bp.Template == "" {
		return bp, nil
	}

	parent, err := bm.resolveRawTemplate(bp.Template, visited)
	if err != nil {
		if errors.Is(err, ErrBlueprintNotFound) {
			return nil, fmt.Errorf("cannot find template '%s' for '%s'", bp.Template, bpName)
		}
		return nil, err
	}

	// Merge YAML nodes
	mergedNode, err := bm.mergeYAMLNodes(parent.Node, bp.Node)
	if err != nil {
		return nil, fmt.Errorf("failed to merge templates for '%s': %w", bpName, err)
	}

	return &RawBlueprint{
		Name:        bp.Name,
		Description: bp.Description,
		Template:    bp.Template,
		IsTemplate:  bp.IsTemplate,
		Node:        mergedNode,
	}, nil
}

// mergeYAMLNodes merges two YAML nodes, preserving CEL expressions.
func (bm *BlueprintManager) mergeYAMLNodes(parent, child *yaml.Node) (*yaml.Node, error) {
	// Instead of decoding to maps (which loses tags), we need to merge at the node level
	return bm.mergeYAMLNodesWithTags(parent, child, "")
}

// mergeYAMLNodesWithTags merges YAML nodes while preserving all tags.
// path tracks the dotted key path for strategy lookup (e.g. "storages.home.claimSpec").
func (bm *BlueprintManager) mergeYAMLNodesWithTags(parent, child *yaml.Node, path string) (*yaml.Node, error) {
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
				childPath := key
				if path != "" {
					childPath = path + "." + key
				}
				mergedValue, err := bm.mergeValueNodes(parentValue, valueNode, key, childPath)
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
				if key != "isTemplate" {
					result.Content = append(result.Content, keyNode, valueNode)
				}
			}
		}
	}

	return result, nil
}

// mergeValueNodes merges two value nodes based on their types and merge strategies.
// key is the immediate key name; path is the full dotted path used for strategy lookup.
func (bm *BlueprintManager) mergeValueNodes(parentValue, childValue *yaml.Node, key, path string) (*yaml.Node, error) {
	if parentValue.Kind == yaml.MappingNode && childValue.Kind == yaml.MappingNode {
		return bm.mergeYAMLNodesWithTags(parentValue, childValue, path)
	}

	if parentValue.Kind == yaml.SequenceNode && childValue.Kind == yaml.SequenceNode {
		return bm.mergeSequenceNodes(parentValue, childValue, key, path)
	}

	return childValue, nil
}

// findStrategy returns the merge strategy for a sequence node by checking:
// 1. Exact full path (e.g. "storages.home.claimSpec.accessModes")
// 2. Suffix match (e.g. "claimSpec.accessModes" matches any path ending with it)
// 3. Bare key name (e.g. "initScripts")
func (bm *BlueprintManager) findStrategy(key, path string) func([]interface{}, []interface{}) []interface{} {
	if s, ok := bm.strategies[path]; ok {
		return s
	}
	for k, s := range bm.strategies {
		if strings.HasSuffix(path, "."+k) {
			return s
		}
	}
	if s, ok := bm.strategies[key]; ok {
		return s
	}
	return nil
}

// mergeSequenceNodes merges two sequence nodes based on the configured strategy.
// Strategy lookup checks for an exact full-path match first, then a suffix match
// (e.g. "claimSpec.accessModes" matches "storages.home.claimSpec.accessModes"),
// then falls back to the bare key name.
func (bm *BlueprintManager) mergeSequenceNodes(parentSeq, childSeq *yaml.Node, key, path string) (*yaml.Node, error) {
	strategy := bm.findStrategy(key, path)
	if strategy != nil {
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
