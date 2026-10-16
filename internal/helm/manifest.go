// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import (
	"fmt"
	"strings"

	sigsyaml "sigs.k8s.io/yaml"
)

// ManifestDocuments decodes a rendered chart manifest into plain objects keyed
// "Kind/name". Secrets are left out and ConfigMaps lose their data and
// binaryData, so the result can be handed to the policy engine.
func ManifestDocuments(manifest string) (map[string]map[string]any, error) {
	docs := make(map[string]map[string]any)
	for _, raw := range strings.Split(manifest, "\n---") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var doc map[string]any
		if err := sigsyaml.Unmarshal([]byte(raw), &doc); err != nil {
			return nil, fmt.Errorf("failed to decode manifest document: %w", err)
		}
		kind, _ := doc["kind"].(string)
		if kind == "" || kind == "Secret" {
			continue
		}
		if kind == "ConfigMap" {
			delete(doc, "data")
			delete(doc, "binaryData")
		}
		name := ""
		if meta, ok := doc["metadata"].(map[string]any); ok {
			name, _ = meta["name"].(string)
		}
		docs[kind+"/"+name] = doc
	}
	return docs, nil
}
