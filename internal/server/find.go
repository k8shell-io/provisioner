// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"

	commonv1 "github.com/k8shell-io/common/pkg/api/gen/go/common/v1"
	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	queryv1 "github.com/k8shell-io/common/pkg/api/gen/go/query/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/userstr"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
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
			Usernames:        req.Usernames,
			Blueprint:        req.Blueprint,
			Organization:     req.Organization,
			WorkspaceName:    req.Workspace,
			InjectNamespaces: p.server.config.InjectNamespaces,
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

	userStr, err := userstr.ParseUserStr(req.Userstr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid userstr format: %v", err)
	}

	canUserStr, err := userStr.Canonicalize()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to canonicalize userstr: %v", err)
	}

	identity := canUserStr.Identity()
	opts := ws.GetWorkspacesOptions{
		Usernames: []string{identity.Username()},
	}

	if userStr.WorkloadName() != "" {
		workloadKind := userStr.WorkloadKind()
		workloadName := userStr.WorkloadName()
		namespace := userStr.Namespace("")
		if namespace != "" && !p.server.config.AllowsInjectionNamespace(namespace) {
			return nil, status.Errorf(codes.PermissionDenied,
				"namespace %s is not allowed for injection", namespace)
		}
		if namespace != "" {
			opts.InjectNamespaces = []string{namespace}
		}
		opts.InjectWorkload = workloadName
		opts.InjectKind = workloadKind
		opts.CanonicalId = canUserStr.CanonicalId()
	} else {
		opts.WorkspaceName = canUserStr.WorkspaceName()
		opts.TargetNamespace = userStr.Namespace(p.server.helm.TargetNamespace())
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

// GetWorkspacesQuerySchema returns the query.v1.Descriptor advertising which
// workspace fields are queryable/sortable via QueryWorkspaces.
func (p *ProvisionerService) GetWorkspacesQuerySchema(_ context.Context,
	_ *provisionerv1.GetWorkspacesQuerySchemaRequest) (*queryv1.Descriptor, error) {
	return ws.WorkspacesQueryDescriptor, nil
}

// QueryWorkspaces retrieves workspaces matching a generic query.v1.Payload,
// as advertised by GetWorkspacesQuerySchema.
func (p *ProvisionerService) QueryWorkspaces(
	ctx context.Context,
	req *provisionerv1.QueryWorkspacesRequest,
) (*provisionerv1.GetWorkspacesResponse, error) {
	workspaces, err := ws.QueryWorkspaces(ctx, p.server.helm, p.server.Identity, p.server.config.InjectNamespaces, req.GetQuery())
	if err != nil {
		if errors.Is(err, models.ErrInvalidParameters) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid query: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "failed to query workspaces: %v", err)
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

// ListInjectNamespaces returns the namespaces the provisioner is configured
// to allow workload injection into. When injection is cluster-wide ("*"),
// it lists the namespaces that currently exist in the cluster.
func (p *ProvisionerService) ListInjectNamespaces(ctx context.Context,
	_ *provisionerv1.ListInjectNamespacesRequest) (*provisionerv1.ListInjectNamespacesResponse, error) {
	if !p.server.config.IsClusterWideInjectionEnabled() {
		return &provisionerv1.ListInjectNamespacesResponse{
			Namespaces: p.server.config.InjectNamespaces,
		}, nil
	}

	nsList, err := p.server.helm.KubeClient().CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list namespaces: %v", err)
	}

	namespaces := make([]string, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		namespaces = append(namespaces, ns.Name)
	}

	return &provisionerv1.ListInjectNamespacesResponse{
		Namespaces:  namespaces,
		ClusterWide: true,
	}, nil
}

// ListInjectWorkloads returns the workloads a workspace can be injected into,
// each reporting whether it already hosts an injected workspace and who owns it.
func (p *ProvisionerService) ListInjectWorkloads(ctx context.Context,
	req *provisionerv1.ListInjectWorkloadsRequest) (*provisionerv1.ListInjectWorkloadsResponse, error) {
	namespaces := p.server.config.InjectNamespaces
	if req.Namespace != "" {
		if !p.server.config.AllowsInjectionNamespace(req.Namespace) {
			return nil, status.Errorf(codes.PermissionDenied,
				"namespace %s is not allowed for injection", req.Namespace)
		}
		namespaces = []string{req.Namespace}
	}
	if len(namespaces) == 0 {
		return &provisionerv1.ListInjectWorkloadsResponse{}, nil
	}

	workloads, err := ws.ListInjectWorkloads(ctx, p.server.helm, namespaces, req.Kinds)
	if err != nil {
		if errors.Is(err, models.ErrInvalidParameters) {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "failed to list inject workloads: %v", err)
	}

	protoWorkloads := make([]*provisionerv1.InjectWorkload, 0, len(workloads))
	for _, w := range workloads {
		protoWorkloads = append(protoWorkloads, &provisionerv1.InjectWorkload{
			Namespace:    w.Namespace,
			Kind:         w.Kind,
			Name:         w.Name,
			Replicas:     w.Replicas,
			Injected:     w.Injected,
			Workspace:    w.Workspace,
			Username:     w.Username,
			Organization: w.Organization,
			Blueprint:    w.Blueprint,
			RepoSource:   w.RepoSource,
			RepoRevision: w.RepoRevision,
		})
	}

	return &provisionerv1.ListInjectWorkloadsResponse{Workloads: protoWorkloads}, nil
}

// GetBlueprints returns the summaries of every blueprint registered in the
// provisioner, regardless of user.
func (p *ProvisionerService) GetBlueprints(_ context.Context,
	_ *provisionerv1.GetBlueprintsRequest,
) (*provisionerv1.GetBlueprintsResponse, error) {

	blueprints := p.server.bpManager.GetBlueprintsSummary()

	var protoBlueprints []*commonv1.BlueprintSummary
	for _, b := range blueprints {
		protoBlueprints = append(protoBlueprints, gapi.BlueprintSummaryToProto(b))
	}

	return &provisionerv1.GetBlueprintsResponse{
		Blueprints: protoBlueprints,
	}, nil
}

// GetBlueprint returns the full raw (unevaluated) spec of a single
// blueprint by name, both merged with its inherited Template and as defined
// directly on the blueprint itself, so callers can tell which fields are
// inherited rather than set on this blueprint.
func (p *ProvisionerService) GetBlueprint(_ context.Context,
	req *provisionerv1.GetBlueprintRequest,
) (*provisionerv1.GetBlueprintResponse, error) {

	raw, err := p.server.bpManager.GetRawBlueprint(req.Name)
	if err != nil {
		if errors.Is(err, blueprint.ErrBlueprintNotFound) {
			return nil, status.Errorf(codes.NotFound, "Blueprint %s not found", req.Name)
		}
		return nil, status.Errorf(codes.Internal, "Failed to get blueprint: %v", err)
	}

	own, err := p.server.bpManager.GetRawBlueprintOwn(req.Name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get blueprint: %v", err)
	}

	template, err := p.server.bpManager.GetBlueprintTemplate(req.Name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get blueprint: %v", err)
	}

	b, err := yaml.Marshal(raw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to marshal blueprint: %v", err)
	}

	ownB, err := yaml.Marshal(own)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to marshal blueprint: %v", err)
	}

	return &provisionerv1.GetBlueprintResponse{
		Blueprint:    b,
		OwnBlueprint: ownB,
		Template:     template,
	}, nil
}
