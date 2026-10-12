// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"time"

	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	"github.com/k8shell-io/common/pkg/authz"
	natsc "github.com/k8shell-io/common/pkg/nats"

	ws "github.com/k8shell-io/provisioner/internal/workspace"
)

// storeThreatDetection evaluates the threat:detect policy for the workspace
// being provisioned and stores the result as the policy part of its entry in
// the threat detection bucket. The policy only selects which attack
// categories worktrace watches, so every failure is logged and otherwise
// ignored: provisioning must not depend on it, and a workspace without an
// entry gets worktrace's default. It is a no-op when authz or the bucket is
// not available.
func (p *ProvisionerService) storeThreatDetection(
	ctx context.Context,
	workspace *ws.Workspace,
	mode authz.WorkspaceProvisionMode,
	injectNamespace, workloadName, workloadKind, standaloneNamespace string,
) {
	if p.server.Authz == nil || p.server.threatDetectionKV == nil {
		return
	}

	namespace := standaloneNamespace
	if mode == authz.WorkspaceProvisionModeInject {
		namespace = injectNamespace
	}
	owner := workspace.Username()
	bp := workspace.Blueprint()

	tokenResp, err := p.server.Identity.IssueUserToken(ctx, &identityv1.IssueUserTokenRequest{
		Username: owner,
		Source:   workspace.UserSource(),
	})
	if err != nil {
		p.log.Error().Err(err).Msgf("threat:detect: failed to issue user token for workspace %s", workspace.Name)
		return
	}

	builder := authz.NewThreatDetectEvalRequest(workspace.Name).
		WithOwner(owner).
		WithMode(mode, namespace).
		WithWorkload(workloadName, workloadKind)
	if bp != nil {
		builder = builder.WithBlueprintName(bp.Name).WithBlueprint(bp)
	}
	evalReq, err := builder.Build()
	if err != nil {
		p.log.Error().Err(err).Msgf("threat:detect: invalid request for workspace %s", workspace.Name)
		return
	}

	req := evalReq.ToProto(tokenResp.GetUserToken())
	req.Package = "threat"
	resp, err := p.server.Authz.Evaluate(ctx, req)
	if err != nil {
		p.log.Error().Err(err).Msgf("threat:detect: evaluation failed for workspace %s", workspace.Name)
		return
	}

	// Allowed is deliberately ignored: only the obligations are consumed.
	result := authz.PolicyResultFromProto(resp)
	detect := ""
	if ob, found := authz.ParseDetectObligation(result.Obligations); found {
		detect = ob.String()
	}

	policy := &natsc.ThreatDetectionPolicy{
		Detect:       detect,
		EvaluatedFor: owner,
		EvaluatedAt:  time.Now().Unix(),
	}
	err = natsc.UpdateThreatDetection(p.server.threatDetectionKV, workspace.Name, func(d *natsc.ThreatDetection) {
		d.Policy = policy
	})
	if err != nil {
		p.log.Error().Err(err).Msgf("threat:detect: failed to store result for workspace %s", workspace.Name)
		return
	}
	p.log.Debug().Msgf("threat:detect: workspace %s detect=%q", workspace.Name, detect)
}

// clearThreatDetection removes the workspace's entry from the threat
// detection bucket, including any override, once the workspace is deleted.
func (p *ProvisionerService) clearThreatDetection(workspaceName string) {
	if p.server.threatDetectionKV == nil {
		return
	}
	err := natsc.UpdateThreatDetection(p.server.threatDetectionKV, workspaceName, func(d *natsc.ThreatDetection) {
		d.Policy, d.Override = nil, nil
	})
	if err != nil {
		p.log.Error().Err(err).Msgf("threat:detect: failed to clear entry for workspace %s", workspaceName)
	}
}
