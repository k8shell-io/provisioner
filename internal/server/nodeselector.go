// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
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
				NodeCount:        int32(v.NodeCount),
				ReadyCount:       int32(v.ReadyCount),
				SchedulableCount: int32(v.SchedulableCount),
			})
		}
		resp.Keys = append(resp.Keys, pk)
	}
	return resp, nil
}
