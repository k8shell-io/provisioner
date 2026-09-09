// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/resource"
)

// UpdateWorkspaceResources changes a workspace's CPU/memory limits and/or its
// network policy class and egress rules directly on the live Kubernetes
// objects, without touching the Helm release. The pod is resized in place via
// the pods/resize subresource and the NetworkPolicies are re-rendered from the
// chart and re-applied. Because the Helm release is left as-is, a later
// re-provision (or a stop followed by a start) reverts the workspace to the
// resources and network class declared in its blueprint.
func (p *ProvisionerService) UpdateWorkspaceResources(ctx context.Context,
	req *provisionerv1.UpdateWorkspaceResourcesRequest) (*provisionerv1.UpdateWorkspaceResourcesResponse, error) {

	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	opts := ws.UpdateOptions{}

	if r := req.Resources; r != nil && (r.Cpu != "" || r.Memory != "") {
		if r.Cpu != "" {
			if _, err := resource.ParseQuantity(r.Cpu); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "invalid cpu quantity %q: %v", r.Cpu, err)
			}
		}
		if r.Memory != "" {
			if _, err := resource.ParseQuantity(r.Memory); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "invalid memory quantity %q: %v", r.Memory, err)
			}
		}
		opts.ChangeResources = true
		opts.CPU = r.Cpu
		opts.Memory = r.Memory
	}

	if n := req.Network; n != nil && (n.NetworkPolicyClass != "" || n.ReplaceEgress) {
		opts.ChangeNetwork = true
		opts.NetworkPolicyClass = n.NetworkPolicyClass
		opts.ReplaceEgress = n.ReplaceEgress
		opts.AllowEgressToCIDRs = n.AllowEgressToCidrs
		for _, sel := range n.AllowEgressToPods {
			if len(sel.MatchLabels) > 0 {
				opts.AllowEgressToPods = append(opts.AllowEgressToPods, sel.MatchLabels)
			}
		}
	}

	if n := req.Network; n != nil && n.ReplaceWebProxy {
		if p := n.WebProxyPort; p != 0 {
			if p < 1 || p > 65535 {
				return nil, status.Errorf(codes.InvalidArgument, "web_proxy_port %d is out of range (1-65535)", p)
			}
			if p == models.WORKSPACE_PORT {
				return nil, status.Errorf(codes.InvalidArgument,
					"web_proxy_port %d is reserved for the workspace grpc API", p)
			}
		}
		opts.ReplaceWebProxy = true
		opts.WebProxyPort = int(n.WebProxyPort)
		opts.WebProxyRoles = n.WebProxyRoles
	}

	if !opts.ChangeResources && !opts.ChangeNetwork && !opts.ReplaceWebProxy {
		return nil, status.Errorf(codes.InvalidArgument,
			"at least one of resources (cpu/memory), network (network_policy_class/replace_egress) "+
				"or web proxy (replace_web_proxy) must be set")
	}

	if _, pod, findErr := ws.FindWorkspace(ctx, p.server.helm, name, p.server.config.InjectNamespaces); findErr == nil &&
		pod.Labels[helm.LabelInjected] == "true" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"workspace %s is injected into a workload and cannot be updated this way", name)
	}

	w, err := ws.NewWorkspaceFromHelmRelease(ctx, name, p.server.helm, p.server.Identity, p.server.config)
	if err != nil {
		return nil, convertToGRPCError(err)
	}

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	workspaceLock := w.CreateLock()
	acquired, err := workspaceLock.TryAcquire(lockCtx)
	if err != nil {
		if errors.Is(err, ws.ErrLockAlreadyHeld) {
			return nil, status.Errorf(codes.Aborted, "another operation on workspace %s is in progress", name)
		}
		return nil, status.Errorf(codes.Internal, "failed to acquire lock for workspace %s: %v", name, err)
	}
	if !acquired {
		return nil, status.Errorf(codes.Aborted, "another operation on workspace %s is in progress", name)
	}
	defer func() {
		if unlockErr := workspaceLock.Release(context.WithoutCancel(ctx)); unlockErr != nil {
			p.log.Error().Err(unlockErr).Msgf("Failed to release lock after updating workspace %s", name)
		}
	}()

	result, err := w.UpdateResourcesAndNetwork(ctx, opts)
	if err != nil {
		return nil, convertToGRPCError(err)
	}

	resp := &provisionerv1.UpdateWorkspaceResourcesResponse{}
	var changes []string
	if result.ResourcesChanged {
		resp.AppliedCpu = result.AppliedCPU
		resp.AppliedMemory = result.AppliedMemory
		changes = append(changes, fmt.Sprintf("resources cpu=%s memory=%s", result.AppliedCPU, result.AppliedMemory))
	}
	if result.NetworkChanged {
		// AppliedNetworkPolicyClass is empty for an egress-only update, which
		// leaves the class untouched — mirror that in the response and message
		// rather than reporting a class the caller never asked to change.
		resp.AppliedNetworkPolicyClass = result.AppliedNetworkPolicyClass
		if result.AppliedNetworkPolicyClass != "" {
			changes = append(changes, fmt.Sprintf("network policy class=%s", result.AppliedNetworkPolicyClass))
		} else {
			changes = append(changes, "network egress rules")
		}
	}
	if result.WebProxyChanged {
		resp.AppliedWebProxyPort = int32(result.AppliedWebProxyPort)
		if result.AppliedWebProxyPort != 0 {
			changes = append(changes, fmt.Sprintf("web proxy port=%d", result.AppliedWebProxyPort))
		} else {
			changes = append(changes, "web proxy route cleared")
		}
	}
	resp.Message = fmt.Sprintf("Workspace %s updated (%s); reverts on the next re-provision",
		name, strings.Join(changes, ", "))

	p.log.Info().Msg(resp.Message)
	return resp, nil
}
