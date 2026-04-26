package blueprint

import (
	"bytes"
	"fmt"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/yaml-cel/pkg/yamlcel"
	"gopkg.in/yaml.v3"
)

// Compose creates a new blueprint by merging a custom blueprint YAML with an existing template
func (bm *BlueprintManager) ComposeRaw(customBlueprint *models.CustomBlueprint) (*RawBlueprint, error) {
	if len(bm.rawBlueprints) == 0 {
		return nil, fmt.Errorf("no blueprints available to compose")
	}

	yamlBytes, err := yaml.Marshal(customBlueprint)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CustomBlueprint to YAML: %w", err)
	}

	var customNode yaml.Node
	if err := yaml.Unmarshal(yamlBytes, &customNode); err != nil {
		return nil, fmt.Errorf("failed to parse custom blueprint YAML: %w", err)
	}

	var contentNode *yaml.Node
	if customNode.Kind == yaml.DocumentNode && len(customNode.Content) > 0 {
		contentNode = customNode.Content[0]
	} else {
		contentNode = &customNode
	}

	if contentNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("custom blueprint must be a YAML mapping/object")
	}

	blueprintFieldNode := contentNode

	var customBlueprintMap map[string]interface{}
	if err := blueprintFieldNode.Decode(&customBlueprintMap); err != nil {
		return nil, fmt.Errorf("failed to decode blueprint content: %w", err)
	}

	templateName, ok := customBlueprintMap["template"].(string)
	if !ok || templateName == "" {
		return nil, fmt.Errorf("custom blueprint must specify a 'template' field")
	}

	bm.mu.RLock()
	template, exists := bm.rawBlueprints[templateName]
	bm.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("template blueprint '%s' not found", templateName)
	}

	blueprintName := templateName
	if name, ok := customBlueprintMap["name"].(string); ok && name != "" {
		blueprintName = name
	}

	mergedNode, err := bm.mergeYAMLNodes(template.Node, blueprintFieldNode)
	if err != nil {
		return nil, fmt.Errorf("failed to compose blueprint using template '%s': %w", templateName, err)
	}

	var composedData interface{}
	if err := mergedNode.Decode(&composedData); err != nil {
		return nil, fmt.Errorf("failed to decode merged blueprint: %w", err)
	}

	return &RawBlueprint{
		Name:     blueprintName,
		Template: "",
		Node:     mergedNode,
	}, nil
}

func (bm *BlueprintManager) Compose(customBlueprint *models.CustomBlueprint) (interface{}, error) {
	rawBlueprint, err := bm.ComposeRaw(customBlueprint)
	if err != nil {
		bm.log.Error().Err(err).Msg("Failed to compose blueprint")
		return nil, err
	}

	clonedNode := bm.cloneAndProcessCELNodes(rawBlueprint.Node)

	var result interface{}
	if err := clonedNode.Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode composed blueprint: %w", err)
	}

	return result, nil
}

func (bm *BlueprintManager) ComposeWithScope(customBlueprint *models.CustomBlueprint,
	scope *BlueprintScope) (*models.Blueprint, error) {
	rawBp, err := bm.ComposeRaw(customBlueprint)
	if err != nil {
		bm.log.Error().Err(err).Msg("Failed to compose blueprint with scope")
		return nil, err
	}

	scope.Metadata.Name = rawBp.Name
	var tmpl yamlcel.CELTemplate
	if err := rawBp.Node.Decode(&tmpl); err != nil {
		return nil, fmt.Errorf("failed to decode CEL template for %s: %w", rawBp.Name, err)
	}

	mapScope, err := scope.ToMap()
	mapScope["blueprint"] = rawBp.Name
	if err != nil {
		return nil, fmt.Errorf("failed to convert scope to map: %w", err)
	}

	docBytes, err := tmpl.EvalToBytes(mapScope, map[string]string{})
	if err != nil {
		return nil, fmt.Errorf("error evaluating CEL template for %s: %w", rawBp.Name, err)
	}

	var bp models.Blueprint
	decoder := yaml.NewDecoder(bytes.NewReader(docBytes))
	decoder.KnownFields(bm.knownFields)
	if err := decoder.Decode(&bp); err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	if bp.Name != "" {
		bp.Name = normalizeDNSLabel(bp.Name)
	}

	v := bp.Validate()
	if !v.IsValid() {
		return nil, fmt.Errorf("blueprint validation failed: %v", v.Errors())
	}

	return &bp, nil
}
