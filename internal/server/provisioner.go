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

// GetUserWorkspaceInfo retrieves all workspaces for a given user and optional blueprint
func (p *ProvisionerService) GetUserWorkspaceInfo(ctx context.Context,
	req *provisionerpb.GetUserWorkspacesRequest) (*commonpb.WorkspaceInfo, error) {

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

	if len(workspaces) == 0 {
		return nil, status.Errorf(codes.NotFound, "No workspaces found for user %s with blueprint %s", username, blueprint)
	}

	if len(workspaces) > 1 {
		return nil, status.Errorf(codes.Internal,
			"multiple workspaces found for user %s with blueprint %s", username, blueprint)
	}

	return gapi.WorkspaceInfoToProto(&workspaces[0]), nil
}

// ListWorkspaces lists all workspaces, optionally filtered by user and/or blueprint
func (p *ProvisionerService) ListWorkspaces(ctx context.Context,
	req *provisionerpb.ListWorkspacesRequest) (*provisionerpb.ListWorkspacesResponse, error) {

	username := req.Username
	blueprint := req.Blueprint

	workspaces, err := ws.GetWorkspaceInfo(p.server.helm, "", username, blueprint)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to list workspaces: %v", err)
	}

	var protoWorkspaces []*commonpb.WorkspaceInfo
	for _, w := range workspaces {
		protoWorkspaces = append(protoWorkspaces, gapi.WorkspaceInfoToProto(&w))
	}

	return &provisionerpb.ListWorkspacesResponse{
		Workspaces: protoWorkspaces,
	}, nil
}

// ProvisionWorkspaceStream provisions a new workspace with streaming updates
func (p *ProvisionerService) ProvisionWorkspaceStream(req *provisionerpb.ProvisionWorkspaceRequest,
	stream provisionerpb.ProvisionerService_ProvisionWorkspaceStreamServer) error {

	ctx := stream.Context()

	userstr, err := models.NewUserStr(req.Userstr)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "Invalid userstr format: %v", err)
	}

	canUserStr, err := userstr.Canonicalize()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "Failed to canonicalize userstr: %v", err)
	}

	workspace, err := p.prepareWorkspaceProvisioning(ctx, canUserStr)
	if err != nil {
		return err
	}

	if err := stream.Send(&provisionerpb.ProvisionEvent{
		Type:       "status",
		Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
		ObjectName: workspace.Name,
		Status:     "Starting",
		Message:    "Provisioning started",
	}); err != nil {
		return err
	}

	messages := make(chan models.WorkspaceStreamEvent, 100)
	done := make(chan *models.PodStatus)
	errorChan := make(chan error)

	timeout := int(req.Timeout)
	if timeout <= 0 {
		timeout = 20
	}

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
					ObjectName: workspace.Name,
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
					ObjectName: workspace.Name,
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
	userStr *models.CanonicalUserStr) (*ws.Workspace, error) {

	userpb, err := p.server.Identity.FindUser(ctx, &identitypb.FindUserRequest{Username: userStr.Identity.Username})
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Failed to get user: %v", err)
	}
	user := gapi.ProtoToUser(userpb)

	var blueprintObj *models.Blueprint
	switch {
	case userStr.Identity.BlueprintKind == models.BlueprintKindCustom:
		blueprintpb, err := p.server.Identity.GetBlueprintByUserStr(ctx, &identitypb.UserStr{Userstr: userStr.CanonicalUserStr})
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to get blueprint by userstr: %v", err)
		}

		var customBlueprint models.CustomBlueprint
		err = json.Unmarshal([]byte(blueprintpb.BlueprintJson), &customBlueprint)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to parse custom blueprint JSON: %v", err)
		}

		if !user.HasBlueprint(customBlueprint.Template) {
			return nil, status.Errorf(codes.PermissionDenied,
				"Access denied: user %s is not authorized to use blueprint's template %s", userStr.Identity.Username, customBlueprint.Template)
		}

		scope, errx := p.server.GetBlueprintScope(customBlueprint.Metadata.Name, user, &customBlueprint.Metadata, userStr.WorkspaceID)
		if errx != nil {
			return nil, p.convertToGRPCError(errx)
		}

		blueprintObj, err = p.server.bpManager.ComposeWithScope(&customBlueprint, scope)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to compose blueprint: %v", err)
		}

		user = scope.User

	case userStr.Identity.BlueprintKind == models.BlueprintKindImplicit || userStr.Identity.BlueprintKind == models.BlueprintKindExplicit:
		bpName := userStr.Identity.Blueprint
		if userStr.Identity.BlueprintKind == models.BlueprintKindImplicit {
			bpName, err = p.server.bpManager.GetDefaultUserBlueprint(user)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"No blueprint specified in userstr and no default blueprint found for user: %v", err)
			}
		}

		if bpName == "" {
			return nil, status.Errorf(codes.InvalidArgument,
				"blueprint name is required in userstr of kind explicit or implicit")
		}

		scope, errx := p.server.GetBlueprintScope(bpName, user, nil, userStr.WorkspaceID)
		if errx != nil {
			return nil, p.convertToGRPCError(errx)
		}

		if !user.HasBlueprint(bpName) {
			return nil, status.Errorf(codes.PermissionDenied,
				"Access denied: user %s is not authorized to use blueprint %s", userStr.Identity.Username, bpName)
		}

		blueprintObj, err = p.server.bpManager.GetBlueprint(bpName, scope)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "Blueprint %s not found", userStr.Identity.Blueprint)
		}

		if blueprintObj.IsTemplate {
			return nil, status.Errorf(codes.InvalidArgument,
				"Blueprint %s is a template and cannot be used to provision a workspace", userStr.Identity.Blueprint)
		}
	}

	workspace, err := ws.NewWorkspace(userStr.WorkspaceID, blueprintObj, user, p.server.helm, p.server.Identity,
		&p.server.config.CertManager, &p.server.config.K8shellCapabilities)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create workspace: %v", err)
	}

	return workspace, nil
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

// DeleteWorkspace deletes a workspace asynchronously with distributed locking
func (p *ProvisionerService) DeleteWorkspace(ctx context.Context,
	req *provisionerpb.Workspace) (*provisionerpb.DeleteWorkspaceResponse, error) {

	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	w, err := ws.NewWorkspaceFromHelmRelease(ctx, name, p.server.helm, p.server.Identity,
		&p.server.config.CertManager)
	if err != nil {
		return nil, p.convertToGRPCError(err)
	}

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	workspaceLock := w.CreateLock()
	acquired, err := workspaceLock.TryAcquire(lockCtx)
	if err != nil {
		if errors.Is(err, ws.ErrLockAlreadyHeld) {
			return &provisionerpb.DeleteWorkspaceResponse{
				Message: fmt.Sprintf("Request to delete the workspace %s already exists", name),
			}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"Failed to acquire lock for workspace %s deletion: %v", name, err)
	}

	if !acquired {
		return &provisionerpb.DeleteWorkspaceResponse{
			Message: fmt.Sprintf("Request to delete the workspace %s already exists", name),
		}, nil
	}

	go func() {
		defer func() {
			unlockCtx := context.Background()
			if unlockErr := workspaceLock.Release(unlockCtx); unlockErr != nil {
				p.log.Error().Err(unlockErr).Msgf("Failed to release lock after deleting workspace %s", name)
			}
		}()

		time.Sleep(2 * time.Second)
		p.log.Debug().Msgf("Starting async deletion of workspace %s", name)

		err := w.Uninstall(context.Background(), time.Duration(10)*time.Second, false, false)
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
