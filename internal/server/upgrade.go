package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (p *ProvisionerService) UpgradeWorkspaceResources(ctx context.Context,
	req *provisionerv1.UpgradeWorkspaceResourcesRequest) (*provisionerv1.UpgradeWorkspaceResourcesResponse, error) {

	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	if req.Cpu == "" && req.Memory == "" {
		return nil, status.Errorf(codes.InvalidArgument, "at least one of cpu or memory must be specified")
	}

	_, pod, err := ws.FindWorkspace(ctx, p.server.helm, name, p.server.config.InjectNamespaces)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return nil, status.Errorf(codes.NotFound, "Workspace %s not found", name)
		}
		return nil, status.Errorf(codes.Internal, "Failed to find workspace: %v", err)
	}

	wl, err := ws.ParseWorkspaceMetadata(pod.Labels, pod.Annotations)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to parse workspace metadata: %v", err)
	}

	userpb, err := p.server.Identity.FindUser(ctx, &identityv1.FindUserRequest{Username: wl.Username})
	if err != nil {
		return nil, fmt.Errorf("failed to get user %s: %w", wl.Username, err)
	}
	user := gapi.ProtoToUser(userpb)

	w, err := ws.NewWorkspace(name, nil, user, wl.UserStr, p.server.helm, p.server.Identity, p.server.config)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create workspace for upgrade: %v", err)
	}

	err = w.ResizeResources(ctx, req.Cpu, req.Memory)
	if err != nil {
		if errors.Is(err, ws.ErrInvalidValue) {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to resize workspace resources: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "Failed to resize workspace resources: %v", err)
	}

	return &provisionerv1.UpgradeWorkspaceResourcesResponse{
		Status:  "Success",
		Message: fmt.Sprintf("Workspace %s resources upgraded successfully", name),
	}, nil

}

func (p *ProvisionerService) UpgradeWorkspaceStream(
	req *provisionerv1.UpgradeWorkspaceRequest,
	stream grpc.ServerStreamingServer[provisionerv1.ProvisionWorkspaceResponse],
) error {
	name := req.Workspace
	msgStream := provisionHandshakeSender{stream}
	if name == "" {
		return p.sendHandshakeErr(msgStream, "", status.Errorf(codes.InvalidArgument,
			"workspace name is required"))
	}

	ctx := stream.Context()
	_, pod, err := ws.FindWorkspace(ctx, p.server.helm, name, p.server.config.InjectNamespaces)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.NotFound,
				"Workspace %s not found", name))
		}
		return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.Internal,
			"Failed to find workspace: %v", err))
	}

	wl, err := ws.ParseWorkspaceMetadata(pod.Labels, pod.Annotations)
	if err != nil {
		return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.Internal,
			"Failed to parse workspace metadata: %v", err))
	}

	_, err = p.server.Identity.FindUser(ctx, &identityv1.FindUserRequest{Username: wl.Username})
	if err != nil {
		return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.Internal,
			"Failed to get user %s: %v", wl.Username, err))
	}

	if !req.Force {
		workspace, err := p.prepareWorkspaceWithPod(ctx, pod)
		if err != nil {
			return p.sendHandshakeErr(msgStream, name, err)
		}

		canUpgrade, err := workspace.CanUpgrade(ctx, pod)
		if err != nil {
			return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.Internal,
				"Failed to check if workspace can be upgraded: %v", err))
		}

		if !canUpgrade {
			return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.FailedPrecondition,
				"Workspace %s cannot be upgraded because it is already up to date.", name))
		}
	}

	_, err = p.DeleteWorkspace(ctx, &provisionerv1.DeleteWorkspaceRequest{
		Workspace:    name,
		DelaySeconds: 0,
	})
	if err != nil {
		return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.Internal,
			"Failed to delete workspace %s for upgrade: %v", name, err))
	}

	// this is temp until we have a better way to coordinate the upgrade process and
	// ensure the workspace is fully deleted before starting provisioning again
	time.Sleep(time.Duration(2) * time.Second)

	return p.ProvisionWorkspaceStream(&provisionerv1.ProvisionWorkspaceRequest{
		Userstr:      wl.UserStr.CanonicalUserStr(),
		Timeout:      req.Timeout,
		SendEvents:   req.SendEvents,
		SendProgress: req.SendProgress,
	}, stream)
}
