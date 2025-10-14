package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/gapi/commonpb"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/identity/pkg/api/identitypb"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"github.com/k8shell-io/provisioner/pkg/api/provisionerpb"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ProvisionerService implements the gRPC service for workspace provisioning
type ProvisionerService struct {
	server *Server
	log    *zerolog.Logger
	provisionerpb.UnimplementedProvisionerServiceServer
}

// NewProvisionerService creates a new instance of the ProvisionerService
func NewProvisionerService(server *Server) *ProvisionerService {
	return &ProvisionerService{
		server: server,
		log:    server.log,
	}
}

// GetWorkspaceStatus retrieves the status of a specific workspace
func (p *ProvisionerService) GetWorkspaceStatus(ctx context.Context,
	req *provisionerpb.Workspace) (*commonpb.WorkspaceStatus, error) {
	status, err := ws.GetWorkspaceStatus(ctx, p.server.helm, req.Workspace)
	if err != nil {
		return nil, err
	}
	return gapi.WorkspaceStatusToProto(status), nil
}

// GetWorkspaces retrieves all workspaces for a given user and optional blueprint
func (p *ProvisionerService) GetWorkspaces(ctx context.Context,
	req *provisionerpb.GetWorkspacesRequest) (*provisionerpb.GetWorkspacesResponse, error) {

	username := req.Username
	blueprint := req.Blueprint

	userpb, err := p.server.Identity.FindUser(ctx, &identitypb.FindUserRequest{Username: username})
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Failed to get user: %v", err)
	}
	user := gapi.ProtoToUser(userpb)

	if blueprint == "" {
		blueprint, err = p.server.bpManager.GetDefaultUserBlueprint(user)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"No blueprint specified and no default blueprint found for user: %v", err)
		}
	}

	workspaces, err := ws.GetWorkspaceInfo(p.server.helm, "", username, blueprint)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get workspace info: %v", err)
	}

	workspaceInfos := make([]*commonpb.WorkspaceInfo, 0, len(workspaces))
	for _, workspace := range workspaces {
		workspaceInfos = append(workspaceInfos, gapi.WorkspaceInfoToProto(&workspace))
	}

	return &provisionerpb.GetWorkspacesResponse{
		Workspaces: workspaceInfos,
	}, nil
}

// ProvisionWorkspaceStream provisions a new workspace with streaming updates
func (p *ProvisionerService) ProvisionWorkspaceStream(req *provisionerpb.ProvisionWorkspaceRequest,
	stream provisionerpb.ProvisionerService_ProvisionWorkspaceStreamServer) error {

	ctx := stream.Context()
	workspace, timeout, err := p.prepareWorkspaceProvisioning(ctx, req)
	if err != nil {
		return err
	}

	if err := stream.Send(&provisionerpb.ProvisionEvent{
		Type:       "status",
		Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
		ObjectName: workspace.Name(),
		Status:     "Starting",
		Message:    "Provisioning started",
	}); err != nil {
		return err
	}

	messages := make(chan models.WorkspaceStreamEvent, 100)
	done := make(chan *models.PodStatus)
	errorChan := make(chan error)

	// Provision the workspace
	go func() {
		defer close(done)
		defer close(errorChan)

		status, err := workspace.Provision(ctx, &ws.ProvisionOptions{
			Timeout:  timeout,
			Messages: messages,
		})

		if err != nil {
			errorChan <- err
			return
		}

		done <- status
	}()

	// Stream events
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case msg, ok := <-messages:
			if !ok {
				continue
			}
			if err := stream.Send(&provisionerpb.ProvisionEvent{
				Type:       "event",
				Timestamp:  msg.Timestamp,
				ObjectName: msg.ObjectName,
				Message:    msg.Message,
			}); err != nil {
				return err
			}

		case status := <-done:
			if status != nil {
				if err := stream.Send(&provisionerpb.ProvisionEvent{
					Type:       "status",
					Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
					ObjectName: workspace.Name(),
					Status:     status.Status,
					Message:    status.Message,
				}); err != nil {
					return err
				}
			}
			return nil

		case err := <-errorChan:
			if err != nil {
				if err := stream.Send(&provisionerpb.ProvisionEvent{
					Type:       "status",
					Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
					ObjectName: workspace.Name(),
					Status:     "Error",
					Message:    err.Error(),
				}); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

// prepareWorkspaceProvisioning handles the common setup logic for workspace provisioning
func (p *ProvisionerService) prepareWorkspaceProvisioning(ctx context.Context,
	req *provisionerpb.ProvisionWorkspaceRequest) (*ws.Workspace, int, error) {

	userstrParam := req.Userstr
	timeout := int(req.Timeout)

	p.log.Debug().Msgf("ProvisionWorkspace called with userstr=%s, timeout=%d",
		userstrParam, timeout)

	if timeout <= 0 {
		timeout = 20 // default timeout
	}

	if userstrParam == "" {
		return nil, 0, status.Errorf(codes.InvalidArgument, "userstr is required")
	}

	userstr, err := models.NewUserStr(userstrParam)
	if err != nil {
		return nil, 0, status.Errorf(codes.InvalidArgument, "Invalid userstr format: %v", err)
	}

	userpb, err := p.server.Identity.FindUser(ctx, &identitypb.FindUserRequest{Username: userstr.Username})
	if err != nil {
		return nil, 0, status.Errorf(codes.NotFound, "Failed to get user: %v", err)
	}
	user := gapi.ProtoToUser(userpb)

	bpName := userstr.Blueprint
	if bpName == "" {
		bpName, err = p.server.bpManager.GetDefaultUserBlueprint(user)
		if err != nil {
			return nil, 0, status.Errorf(codes.InvalidArgument,
				"No blueprint specified in userstr and no default blueprint found for user: %v", err)
		}
	}

	var blueprintObj *models.Blueprint

	if userstr.HasCustomBlueprint {
		blueprintpb, err := p.server.Identity.GetBlueprintByUserStr(ctx, &identitypb.UserStr{Userstr: userstrParam})
		if err != nil {
			return nil, 0, status.Errorf(codes.InvalidArgument,
				"No blueprint was provided, and no default blueprint is configured for user %s", userstr.Username)
		}

		var customBlueprint models.CustomBlueprint
		err = json.Unmarshal([]byte(blueprintpb.BlueprintJson), &customBlueprint)
		if err != nil {
			return nil, 0, status.Errorf(codes.Internal, "Failed to parse custom blueprint JSON: %v", err)
		}

		if !user.HasBlueprint(customBlueprint.Template) {
			return nil, 0, status.Errorf(codes.PermissionDenied,
				"Access denied: user %s is not authorized to use blueprint's template %s", userstr.Username, customBlueprint.Template)
		}

		scope, errx := p.server.GetBlueprintScope("", user, &customBlueprint.Metadata)
		if errx != nil {
			return nil, 0, p.convertToGRPCError(errx)
		}

		blueprintObj, err = p.server.bpManager.ComposeWithScope(&customBlueprint, scope)
		if err != nil {
			return nil, 0, status.Errorf(codes.InvalidArgument, "Failed to compose blueprint: %v", err)
		}

		user = scope.User
	} else {
		scope, errx := p.server.GetBlueprintScope(bpName, user, nil)
		if errx != nil {
			return nil, 0, p.convertToGRPCError(errx)
		}

		if !user.HasBlueprint(bpName) {
			return nil, 0, status.Errorf(codes.PermissionDenied,
				"Access denied: user %s is not authorized to use blueprint %s", userstr.Username, bpName)
		}

		blueprintObj, err = p.server.bpManager.GetBlueprint(bpName, scope)
		if err != nil {
			return nil, 0, status.Errorf(codes.NotFound, "Blueprint %s not found", userstr.Blueprint)
		}

		if blueprintObj.IsTemplate {
			return nil, 0, status.Errorf(codes.InvalidArgument,
				"Blueprint %s is a template and cannot be used to provision a workspace", userstr.Blueprint)
		}
	}

	workspace, err := ws.NewWorkspace(blueprintObj, user, p.server.helm, p.server.Identity, p.server.Session)
	if err != nil {
		return nil, 0, status.Errorf(codes.Internal, "Failed to create workspace: %v", err)
	}

	return workspace, timeout, nil
}

// convertToGRPCError converts internal errors to gRPC status errors
func (p *ProvisionerService) convertToGRPCError(err error) error {
	if errors.Is(err, models.ErrWorkspaceNotFound) {
		return status.Errorf(codes.NotFound, "%s", err.Error())
	}
	if errors.Is(err, models.ErrInvalidParameters) {
		return status.Errorf(codes.InvalidArgument, "%s", err.Error())
	}

	return status.Errorf(codes.Internal, "%s", err.Error())
}

// DeleteWorkspace deletes a workspace asynchronously
func (p *ProvisionerService) DeleteWorkspace(ctx context.Context,
	req *provisionerpb.Workspace) (*provisionerpb.DeleteWorkspaceResponse, error) {

	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	w, err := ws.NewWorkspaceFromHelmRelease(ctx, name, p.server.helm, p.server.Identity)
	if err != nil {
		return nil, p.convertToGRPCError(err)
	}

	asyncCtx := context.Background()
	go func() {
		time.Sleep(2 * time.Second)
		p.log.Debug().Msgf("Starting async deletion of workspace %s", name)
		err := w.Uninstall(asyncCtx, time.Duration(10)*time.Second, false)
		if err != nil {
			p.log.Error().Err(err).Msgf("Failed to delete workspace %s", name)
		} else {
			p.log.Info().Msgf("Successfully deleted workspace %s", name)
		}
	}()

	return &provisionerpb.DeleteWorkspaceResponse{
		Message: fmt.Sprintf("Request to delete the workspace %s was submitted", name),
	}, nil
}
