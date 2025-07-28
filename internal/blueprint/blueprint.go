package blueprint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/k8shell-io/yaml-config/pkg/yamlconfig"
	"github.com/rs/zerolog/log"
)

// Blueprint represents the loaded blueprint from YAML.
type Blueprint struct {
	Name     string `yaml:"name"`
	Template string `yaml:"template,omitempty"`
	Raw      map[string]interface{}
}

// MergeStrategies allow custom list merging strategies per dotted path.
type MergeStrategies map[string]func(dst, src []interface{}) []interface{}

// mergeMaps recursively merges `src` into `dst`. Modifies dst.
func mergeMaps(dst, src map[string]interface{}, path []string, strategies MergeStrategies) map[string]interface{} {
	for key, value := range src {
		currentPath := append(path, key)
		pathStr := strings.Join(currentPath, ".")

		if vMap, ok := value.(map[string]interface{}); ok {
			if dstMap, exists := dst[key].(map[string]interface{}); exists {
				dst[key] = mergeMaps(dstMap, vMap, currentPath, strategies)
			} else {
				dst[key] = mergeMaps(make(map[string]interface{}), vMap, currentPath, strategies)
			}
		} else if vList, ok := value.([]interface{}); ok {
			if strategy, found := strategies[pathStr]; found {
				existing, _ := dst[key].([]interface{})
				dst[key] = strategy(existing, vList)
			} else {
				existing, _ := dst[key].([]interface{})
				dst[key] = append(existing, vList...)
			}
		} else {
			dst[key] = value
		}
	}
	return dst
}

// resolveTemplate recursively resolves templates in blueprints.
func ResolveTemplate(bpName string, all map[string]*Blueprint, strategies MergeStrategies,
	visited map[string]bool) (*Blueprint, error) {
	if visited[bpName] {
		return nil, fmt.Errorf("circular template reference: %s", bpName)
	}
	visited[bpName] = true

	bp, found := all[bpName]
	if !found {
		return nil, fmt.Errorf("blueprint %s not found", bpName)
	}
	if bp.Template == "" {
		return bp, nil
	}
	parent, err := ResolveTemplate(bp.Template, all, strategies, visited)
	if err != nil {
		return nil, err
	}
	merged := mergeMaps(deepCopyMap(parent.Raw), bp.Raw, []string{}, strategies)
	return &Blueprint{
		Name:     bp.Name,
		Template: "",
		Raw:      merged,
	}, nil
}

// deepCopyMap creates a deep copy of a map[string]interface{}.
func deepCopyMap(src map[string]interface{}) map[string]interface{} {
	b, _ := json.Marshal(src)
	var dst map[string]interface{}
	json.Unmarshal(b, &dst)
	return dst
}

// LoadBlueprints loads all blueprints from the specified directory.
// It processes YAML files, resolves templates, and returns a map of blueprints.
func LoadBlueprints(dir string) (map[string]*Blueprint, error) {
	blueprintsRaw := make(map[string]*Blueprint)

	p := yamlconfig.NewProcessor(
		yamlconfig.ProcessorOptions{
			EnableEnvVarExpansion: true,
			EnableFileTag:         true,
			RequireEnvVars:        true,
		},
	)

	// load all YAML files in the directory
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml" {
			return nil
		}

		root, err := p.LoadFile(path)
		if err != nil {
			log.Error().Err(err).Str("path", path).Msg("Failed to load blueprint file")
			return err
		}

		var top map[string]interface{}
		if err := root.Decode(&top); err != nil {
			log.Error().Err(err).Str("path", path).Msg("Failed to decode YAML to map")
			return nil
		}

		switch {
		case top["blueprint"] != nil:
			if bpMap, ok := top["blueprint"].(map[string]interface{}); ok {
				bp := &Blueprint{
					Name:     generateRandomName("bp"),
					Template: "",
					Raw:      bpMap,
				}
				if n, ok := bpMap["name"].(string); ok {
					bp.Name = n
				}
				if t, ok := bpMap["template"].(string); ok {
					bp.Template = t
				}
				blueprintsRaw[bp.Name] = bp
			} else {
				log.Error().Str("path", path).Msg("Expected blueprint to be a map.")
				return fmt.Errorf("expected blueprint to be a map at %s", path)
			}

		case top["blueprints"] != nil:
			list, ok := top["blueprints"].([]interface{})
			if !ok {
				log.Error().Str("path", path).Msg("Expected blueprints to be a list.")
				return fmt.Errorf("expected blueprints to be a list at %s", path)
			}
			for i, item := range list {
				bpMap, ok := item.(map[string]interface{})
				if !ok {
					log.Warn().Str("path", path).Int("index", i).Msg("Skipping non-object in blueprints list")
					continue
				}
				var name string
				if n, ok := bpMap["name"].(string); ok {
					name = n
				} else {
					name = generateRandomName("bp")
				}
				bp := &Blueprint{
					Name:     name,
					Template: "",
					Raw:      bpMap,
				}
				if t, ok := bpMap["template"].(string); ok {
					bp.Template = t
				}
				blueprintsRaw[bp.Name] = bp
			}

		default:
			log.Warn().Str("path", path).Msg("No blueprint or blueprints key found")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Apply template resolution
	resolved := make(map[string]*Blueprint)
	for name := range blueprintsRaw {
		bp, err := ResolveTemplate(name, blueprintsRaw, MergeStrategies{}, map[string]bool{})
		if err != nil {
			return nil, fmt.Errorf("error resolving blueprint %s: %w", name, err)
		}
		resolved[name] = bp
	}
	return resolved, nil
}

func generateRandomName(prefix string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return prefix + "-xxxx"
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b))
}
