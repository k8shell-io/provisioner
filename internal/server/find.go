package server

import (
	"context"
	"errors"

	commonv1 "github.com/k8shell-io/common/pkg/api/gen/go/common/v1"
	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/userstr"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FindWorkspace retrieves the details of a specific workspace
func (p *ProvisionerService) FindWorkspace(ctx context.Context,
	req *provisionerv1.FindWorkspaceRequest) (*commonv1.WorkspaceDetails, error) {
	s, _, err := ws.FindWorkspace(ctx, p.server.helm, req.Workspace, p.server.config.InjectNamespaces)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return nil, status.Errorf(codes.NotFound, "Workspace %s not found", req.Workspace)
		}
		return nil, status.Errorf(codes.Internal, "Failed to get workspace details: %v", err)
	}

	if p.server.provisionJobsKV != nil && s.JobId != "" {
		_, err := p.server.provisionJobsKV.Get(s.JobId)
		if err != nil {
			s.JobId = ""
		}
	}

	return gapi.WorkspaceDetailsToProto(s), nil
}

// GetWorkspaces lists all workspaces, optionally filtered by user and/or blueprint
func (p *ProvisionerService) GetWorkspaces(
	ctx context.Context,
	req *provisionerv1.GetWorkspacesRequest,
) (*provisionerv1.GetWorkspacesResponse, error) {

	if req.RepoName != "" || req.RepoOwner != "" || req.RepoRef != "" {
		return nil, status.Error(codes.Unimplemented, "Filtering by repo details (RepoName, RepoOwner, RepoRef) is not supported")
	}

	workspaces, err := ws.GetWorkspaces(ctx, p.server.helm,
		ws.GetWorkspacesOptions{
			Username:            req.Username,
			Blueprint:           req.Blueprint,
			Organization:        req.Organization,
			Workspace:           req.Workspace,
			InjectionNamespaces: p.server.config.InjectNamespaces,
		})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to list workspaces: %v", err)
	}

	if p.server.provisionJobsKV != nil {
		for _, w := range workspaces.Workspaces {
			if w.JobId != "" {
				_, err := p.server.provisionJobsKV.Get(w.JobId)
				if err != nil {
					w.JobId = ""
				}
			}
		}
	}

	var protoWorkspaces []*commonv1.WorkspaceDetails
	for _, w := range workspaces.Workspaces {
		protoWorkspaces = append(protoWorkspaces, gapi.WorkspaceDetailsToProto(w))
	}

	return &provisionerv1.GetWorkspacesResponse{
		Workspaces: protoWorkspaces,
	}, nil
}

// GetWorkspacesByUserStr returns workspaces matching the given userstr
func (p *ProvisionerService) GetWorkspacesByUserStr(
	ctx context.Context,
	req *provisionerv1.GetWorkspacesByUserStrRequest,
) (*provisionerv1.GetWorkspacesResponse, error) {
	if req.Userstr == "" {
		return nil, status.Errorf(codes.InvalidArgument, "userstr is required")
	}

	parsedUserStr, err := userstr.ParseUserStr(req.Userstr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid userstr format: %v", err)
	}

	canUserStr, err := parsedUserStr.Canonicalize()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to canonicalize userstr: %v", err)
	}

	identity := canUserStr.Identity()
	opts := ws.GetWorkspacesOptions{
		Username:            identity.Username(),
		InjectionNamespaces: p.server.config.InjectNamespaces,
	}

	if parsedUserStr.Deploy() == "" {
		opts.Workspace = canUserStr.WorkspaceName()
	} else {
		deployName := parsedUserStr.Deploy()
		namespace := parsedUserStr.Namespace("")
		if namespace != "" && !p.server.config.AllowsInjectionNamespace(namespace) {
			return nil, status.Errorf(codes.PermissionDenied,
				"namespace %s is not allowed for injection", namespace)
		}
		if namespace != "" {
			opts.InjectionNamespaces = []string{namespace}
		}
		opts.InjectTarget = deployName
	}

	workspaces, err := ws.GetWorkspaces(ctx, p.server.helm, opts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list workspaces: %v", err)
	}

	var protoWorkspaces []*commonv1.WorkspaceDetails
	for _, w := range workspaces.Workspaces {
		protoWorkspaces = append(protoWorkspaces, gapi.WorkspaceDetailsToProto(w))
	}

	return &provisionerv1.GetWorkspacesResponse{
		Workspaces: protoWorkspaces,
	}, nil
}

func (p *ProvisionerService) GetUserBlueprints(ctx context.Context,
	req *provisionerv1.GetUserBlueprintsRequest,
) (*provisionerv1.GetUserBlueprintsResponse, error) {

	userpb, err := p.server.Identity.FindUser(ctx, &identityv1.FindUserRequest{Username: req.Username})
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Failed to get user: %v", err)
	}
	user := gapi.ProtoToUser(userpb)

	allblueprints := p.server.bpManager.GetBlueprintsSummary()
	var blueprints []*models.BlueprintSummary
	for _, bp := range allblueprints {
		if user.HasBlueprint(bp.Name) {
			blueprints = append(blueprints, bp)
		}
	}

	var protoBlueprints []*commonv1.BlueprintSummary
	for _, b := range blueprints {
		protoBlueprints = append(protoBlueprints, gapi.BlueprintSummaryToProto(b))
	}

	return &provisionerv1.GetUserBlueprintsResponse{
		Blueprints: protoBlueprints,
	}, nil
}

func (p *ProvisionerService) CanUpgradeWorkspace(ctx context.Context,
	req *provisionerv1.CanUpgradeWorkspaceRequest,
) (*provisionerv1.CanUpgradeWorkspaceResponse, error) {
	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	pod, err := p.server.helm.KubeClient().CoreV1().Pods(
		p.server.helm.TargetNamespace()).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get workspace pod: %v", err)
	}

	workspace, err := p.prepareWorkspaceWithPod(ctx, pod)
	if err != nil {
		return nil, err
	}

	canUpgrade, err := workspace.CanUpgrade(ctx, pod)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to check if workspace can be upgraded: %v", err)
	}

	message := "Workspace can be upgraded"
	if !canUpgrade {
		message = "Workspace is up to date."
	}

	return &provisionerv1.CanUpgradeWorkspaceResponse{
		CanUpgrade: canUpgrade,
		Message:    message,
	}, nil
}
