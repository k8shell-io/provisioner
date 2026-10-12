// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	authzv1 "github.com/k8shell-io/common/pkg/api/gen/go/authz/v1"
	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/models"
	natsc "github.com/k8shell-io/common/pkg/nats"

	ws "github.com/k8shell-io/provisioner/internal/workspace"
)

// threatDetectParams identifies the workspace a threat:detect policy is
// evaluated for.
type threatDetectParams struct {
	workspace string
	owner     string
	// blueprint is the blueprint being provisioned, after policy patches.
	blueprint *models.Blueprint
	mode      authz.WorkspaceProvisionMode
	// namespace is the target namespace for standalone workspaces and the
	// workload's namespace for injected ones.
	namespace    string
	workloadName string
	workloadKind string
}

// threatDetection returns the threat detection bucket, opening it on first
// use and again after a failure. The worktrace controller owns the bucket, so
// it is never created here; nil means NATS is disabled or worktrace is not
// installed (or not started yet), and there is nothing to write.
func (p *ProvisionerService) threatDetection() *natsc.JetStreamKV {
	s := p.server
	if s.nats == nil {
		return nil
	}
	s.threatDetectionKVMu.Lock()
	defer s.threatDetectionKVMu.Unlock()
	if s.threatDetectionKV != nil {
		return s.threatDetectionKV
	}
	kv, err := s.nats.OpenKV(natsc.THREAT_DETECTION_BUCKET)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			p.log.Debug().Msg("Threat detection bucket not found; worktrace is not installed or not started")
		} else {
			p.log.Error().Err(err).Msg("Failed to open threat detection bucket")
		}
		return nil
	}
	s.threatDetectionKV = kv
	return kv
}

// storeThreatDetection evaluates the threat:detect policy for the workspace
// being (re)provisioned and stores the result as the policy part of its entry
// in the threat detection bucket. The policy is evaluated for the workspace
// owner with the patched blueprint, so it also works for restarts that no
// userstr drives. The policy only selects which attack categories worktrace
// watches, so every failure is logged and otherwise ignored: provisioning
// must not depend on it, and the existing entry is left untouched. It is a
// no-op when authz or the bucket is not available.
func (p *ProvisionerService) storeThreatDetection(
	ctx context.Context,
	workspace *ws.Workspace,
	mode authz.WorkspaceProvisionMode,
	injectNamespace, workloadName, workloadKind, standaloneNamespace string,
) {
	if p.server.Authz == nil {
		return
	}
	kv := p.threatDetection()
	if kv == nil {
		return
	}

	owner := workspace.Username()
	params := threatDetectParams{
		workspace: workspace.Name,
		owner:     owner,
		blueprint: workspace.Blueprint(),
		mode:      mode,
		namespace: standaloneNamespace,
	}
	if mode == authz.WorkspaceProvisionModeInject {
		params.namespace = injectNamespace
		params.workloadName = workloadName
		params.workloadKind = workloadKind
	}

	tokenResp, err := p.server.Identity.IssueUserToken(ctx, &identityv1.IssueUserTokenRequest{
		Username: owner,
		Source:   workspace.UserSource(),
	})
	if err != nil {
		p.log.Error().Err(err).Msgf("threat:detect: failed to issue user token for workspace %s", workspace.Name)
		return
	}

	result, err := evaluateThreatDetect(ctx, p.server.Authz, tokenResp.GetUserToken(), params)
	if err != nil {
		p.log.Error().Err(err).Msgf("threat:detect: evaluation failed for workspace %s", workspace.Name)
		return
	}
	ob, found := authz.ParseDetectObligation(result.Obligations)

	err = natsc.UpdateThreatDetection(kv, workspace.Name, applyDetectPolicy(ob, found, owner, time.Now()))
	if err != nil {
		p.log.Error().Err(err).Msgf("threat:detect: failed to store result for workspace %s", workspace.Name)
		return
	}
	p.log.Debug().Msgf("threat:detect: workspace %s detect=%q", workspace.Name, ob.String())
}

// evaluateThreatDetect calls authz for the threat:detect action. The result's
// Allowed flag is meaningless for this action; only its obligations are used.
func evaluateThreatDetect(ctx context.Context, client authzv1.AuthzServiceClient, token string,
	params threatDetectParams) (*authz.PolicyResult, error) {

	builder := authz.NewThreatDetectEvalRequest(params.workspace).
		WithOwner(params.owner).
		WithMode(params.mode, params.namespace).
		WithWorkload(params.workloadName, params.workloadKind)
	if params.blueprint != nil {
		builder = builder.WithBlueprintName(params.blueprint.Name).WithBlueprint(params.blueprint)
	}
	req, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("build threat:detect request: %w", err)
	}

	protoReq := req.ToProto(token)
	protoReq.Package = "threat"
	resp, err := client.Evaluate(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("evaluate threat:detect: %w", err)
	}
	return authz.PolicyResultFromProto(resp), nil
}

// applyDetectPolicy returns the mutation that sets the entry's Policy from a
// parsed detect obligation, or clears it when the policy returned none so
// worktrace falls back to its default. It leaves Override alone.
func applyDetectPolicy(ob authz.DetectObligation, found bool, subject string, now time.Time) func(*natsc.ThreatDetection) {
	return func(d *natsc.ThreatDetection) {
		d.Policy = nil
		if found {
			d.Policy = &natsc.ThreatDetectionPolicy{
				Detect:       ob.String(),
				EvaluatedFor: subject,
				EvaluatedAt:  now.Unix(),
			}
		}
	}
}

// clearThreatDetection removes the workspace's entry, including any override,
// so a later workspace with the same name does not inherit it. Call it when a
// workspace is deleted or ejected, not when it is stopped. Failures are only
// logged.
func (p *ProvisionerService) clearThreatDetection(workspaceName string) {
	if workspaceName == "" {
		return
	}
	kv := p.threatDetection()
	if kv == nil {
		return
	}
	if err := kv.Delete(workspaceName); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		p.log.Error().Err(err).Msgf("threat:detect: failed to clear entry for workspace %s", workspaceName)
	}
}
