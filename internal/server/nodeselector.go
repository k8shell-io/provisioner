// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"fmt"
	"sort"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/provisioner/internal/nodepool"
)

// GetNodeSelectorOptions returns the configured node selector keys with the
// values discovered on cluster nodes and per-value node counts.
func (p *ProvisionerService) GetNodeSelectorOptions(ctx context.Context,
	_ *provisionerv1.GetNodeSelectorOptionsRequest) (*provisionerv1.GetNodeSelectorOptionsResponse, error) {

	resp := &provisionerv1.GetNodeSelectorOptionsResponse{}
	for _, k := range p.server.nodePools.Options() {
		pk := &provisionerv1.NodeSelectorKey{Key: k.Key}
		for _, v := range k.Values {
			pk.Values = append(pk.Values, &provisionerv1.NodeSelectorValue{
				Value:            v.Value,
				NodeCount:        v.NodeCount,
				ReadyCount:       v.ReadyCount,
				SchedulableCount: v.SchedulableCount,
			})
		}
		resp.Keys = append(resp.Keys, pk)
	}
	return resp, nil
}

// validateNodeSelectors checks every selector against the configured
// nodeSelectorKeys and the values currently present on cluster nodes, the same
// options GetNodeSelectorOptions reports. A selector whose key is not
// configured, or any of whose values no node carries, is rejected.
func validateNodeSelectors(selectors map[string][]string, options []nodepool.Key) error {
	byKey := make(map[string]map[string]struct{}, len(options))
	for _, k := range options {
		vals := make(map[string]struct{}, len(k.Values))
		for _, v := range k.Values {
			vals[v.Value] = struct{}{}
		}
		byKey[k.Key] = vals
	}

	keys := make([]string, 0, len(selectors))
	for k := range selectors {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		vals, ok := byKey[k]
		if !ok {
			return fmt.Errorf("key %q is not a configured node selector key", k)
		}
		for _, v := range selectors[k] {
			if _, ok := vals[v]; !ok {
				return fmt.Errorf("no node has %s=%s", k, v)
			}
		}
	}
	return nil
}
