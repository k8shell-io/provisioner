// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// revisionLen is the number of hex characters kept from the content hash.
// 12 characters (48 bits) is ample for telling revisions of one blueprint
// apart and fits comfortably in a Kubernetes label value.
const revisionLen = 12

// revisionSalt is mixed into every revision hash. Bump it when merge
// semantics change in code (e.g. a new MergeStrategies entry) so revisions
// change even though no blueprint YAML did.
const revisionSalt = "k8shell-blueprint-revision/v1"

// revisionIgnoredKeys are top-level blueprint keys that do not affect the
// provisioned workspace and so must not change its revision.
var revisionIgnoredKeys = map[string]bool{
	"description": true,
	"isTemplate":  true,
}

// computeRevision returns a short content hash of a fully merged blueprint
// node. Because the node already includes everything inherited from the
// template chain, a change in any ancestor changes the revision of every
// descendant whose effective content changes, and only those.
//
// The hash is canonical: mapping key order, comments, scalar quoting style
// and source positions do not affect it, while tags (including `!cel`) and
// sequence order do.
func computeRevision(node *yaml.Node) string {
	h := sha256.New()
	h.Write([]byte(revisionSalt))
	if node != nil && node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	hashRevisionNode(h, node, true)
	return hex.EncodeToString(h.Sum(nil))[:revisionLen]
}

// hashRevisionNode writes a canonical, length-prefixed encoding of node into
// h. top is true only for the blueprint's root mapping, where
// revisionIgnoredKeys are skipped.
func hashRevisionNode(h hash.Hash, node *yaml.Node, top bool) {
	if node == nil {
		writeRevisionField(h, "nil")
		return
	}
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}

	switch node.Kind {
	case yaml.DocumentNode:
		writeRevisionField(h, "doc")
		for _, c := range node.Content {
			hashRevisionNode(h, c, top)
		}
	case yaml.MappingNode:
		type pair struct {
			key   string
			value *yaml.Node
		}
		pairs := make([]pair, 0, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if top && revisionIgnoredKeys[key] {
				continue
			}
			pairs = append(pairs, pair{key, node.Content[i+1]})
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
		writeRevisionField(h, "map")
		writeRevisionLen(h, len(pairs))
		for _, p := range pairs {
			writeRevisionField(h, p.key)
			hashRevisionNode(h, p.value, false)
		}
	case yaml.SequenceNode:
		writeRevisionField(h, "seq")
		writeRevisionLen(h, len(node.Content))
		for _, c := range node.Content {
			hashRevisionNode(h, c, false)
		}
	default:
		writeRevisionField(h, "scalar")
		writeRevisionField(h, node.ShortTag())
		writeRevisionField(h, node.Value)
	}
}

func writeRevisionField(h hash.Hash, s string) {
	writeRevisionLen(h, len(s))
	h.Write([]byte(s))
}

func writeRevisionLen(h hash.Hash, n int) {
	h.Write([]byte(strconv.Itoa(n) + ":"))
}
