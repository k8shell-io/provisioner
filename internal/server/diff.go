// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"

	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/utils"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/helm"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DiffWorkspaceBlueprint compares the blueprint a workspace was provisioned
// from, recovered from its stored Helm values, with what that blueprint
// resolves to now for the workspace's owner, and returns the result as
// aligned side-by-side rows.
func (p *ProvisionerService) DiffWorkspaceBlueprint(ctx context.Context,
	req *provisionerv1.DiffWorkspaceBlueprintRequest) (*provisionerv1.DiffWorkspaceBlueprintResponse, error) {

	name := req.GetWorkspace()
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	rel, err := ws.FindWorkspaceHelmRelease(ctx, p.server.helm, name)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return nil, status.Errorf(codes.NotFound, "Workspace %s not found", name)
		}
		return nil, convertToGRPCError(err)
	}

	values := rel.Config
	deployed, err := ws.BlueprintFromValues(values)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to recover workspace blueprint: %v", err)
	}
	if deployed.Metadata.RepoName != "" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"workspace %s was provisioned from a repository's k8shell file and has no blueprint to compare with", name)
	}

	username := rel.Labels[helm.LabelUsername]
	bpName := rel.Labels[helm.LabelBlueprint]
	if bpName == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "workspace %s does not record its blueprint", name)
	}

	userpb, err := p.server.Identity.FindUser(ctx, &identityv1.FindUserRequest{Username: username})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get user %s: %v", username, err)
	}
	user := gapi.ProtoToUser(userpb)

	scope, err := p.server.GetBlueprintScope(bpName, user, nil, name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to build blueprint scope: %v", err)
	}
	current, err := p.server.bpManager.GetBlueprint(bpName, scope)
	if err != nil {
		if errors.Is(err, blueprint.ErrBlueprintNotFound) {
			return nil, status.Errorf(codes.NotFound, "Blueprint %s not found", bpName)
		}
		return nil, status.Errorf(codes.Internal, "failed to evaluate blueprint %s: %v", bpName, err)
	}

	// Replay the policy patches recorded at provision time rather than
	// evaluating authz, which is the api-server's job; this way obligations
	// don't show up as removed.
	patches, err := ws.AppliedPatchesFromValues(values)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to read applied policy patches: %v", err)
	}
	current, err = authz.ApplyProvisionPatches(current, patches)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to apply policy patches to blueprint: %v", err)
	}

	envResp, err := p.server.Identity.ListUserEnvVars(ctx, &identityv1.ListUserEnvVarsRequest{Username: username})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list environment variables for user %s: %v", username, err)
	}
	userEnvKeys := make([]string, 0, len(envResp.GetEnvVars()))
	for _, ev := range envResp.GetEnvVars() {
		userEnvKeys = append(userEnvKeys, ev.GetKey())
	}

	diff, err := ws.DiffBlueprints(deployed, current, ws.DiffOptions{
		WorkspaceName: name,
		Organization:  user.Organization,
		UserEnvKeys:   userEnvKeys,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to diff blueprints: %v", err)
	}

	rows := make([]*provisionerv1.BlueprintDiffRow, 0, len(diff.Rows))
	for _, r := range diff.Rows {
		rows = append(rows, &provisionerv1.BlueprintDiffRow{
			Op:        diffOpToProto(r.Op),
			LeftLine:  utils.SafeIntToInt32(r.LeftLine),
			LeftText:  r.LeftText,
			RightLine: utils.SafeIntToInt32(r.RightLine),
			RightText: r.RightText,
		})
	}

	return &provisionerv1.DiffWorkspaceBlueprintResponse{
		Identical:         diff.Identical,
		WorkspaceRevision: rel.Labels[helm.LabelBlueprintRevision],
		BlueprintRevision: p.server.bpManager.GetBlueprintRevision(user.Organization, bpName),
		WorkspaceYaml:     diff.WorkspaceYAML,
		BlueprintYaml:     diff.BlueprintYAML,
		Rows:              rows,
		Added:             utils.SafeIntToInt32(diff.Added),
		Removed:           utils.SafeIntToInt32(diff.Removed),
		Changed:           utils.SafeIntToInt32(diff.Changed),
	}, nil
}

func diffOpToProto(op ws.DiffOp) provisionerv1.BlueprintDiffOp {
	switch op {
	case ws.DiffEqual:
		return provisionerv1.BlueprintDiffOp_BLUEPRINT_DIFF_OP_EQUAL
	case ws.DiffRemoved:
		return provisionerv1.BlueprintDiffOp_BLUEPRINT_DIFF_OP_REMOVED
	case ws.DiffAdded:
		return provisionerv1.BlueprintDiffOp_BLUEPRINT_DIFF_OP_ADDED
	case ws.DiffChanged:
		return provisionerv1.BlueprintDiffOp_BLUEPRINT_DIFF_OP_CHANGED
	default:
		return provisionerv1.BlueprintDiffOp_BLUEPRINT_DIFF_OP_UNSPECIFIED
	}
}
