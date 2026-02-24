package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/gapi/commonpb"
	"github.com/k8shell-io/common/pkg/models"
	natsc "github.com/k8shell-io/common/pkg/nats"
	"github.com/k8shell-io/identity/pkg/api/identitypb"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"github.com/k8shell-io/provisioner/pkg/api/provisionerpb"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const TOTAL_PROVISION_EVENTS = 12

type ProvisionJobServer struct {
	models.ProvisionJob `json:",inline"`
	NextEventId         int64              `json:"-"`
	kv                  *natsc.JetStreamKV `json:"-"`
	log                 *zerolog.Logger    `json:"-"`
}

// NewProvisionJob creates a new ProvisionJob instance with the given username and monitor URL.
func NewProvisionJob(WorkspaceName string, username string, kv *natsc.JetStreamKV, log *zerolog.Logger) *ProvisionJobServer {
	now := time.Now().UTC()
	p := &ProvisionJobServer{
		ProvisionJob: models.ProvisionJob{
			ID:            uuid.NewString(),
			WorkspaceName: WorkspaceName,
			Username:      username,
			Status:        models.ProvisionJobRunning,
			CreatedAt:     now,
			UpdatedAt:     now,
			Events:        []models.WorkspaceStreamEvent{},
		},
		NextEventId: 1,
		kv:          kv,
		log:         log,
	}
	return p
}

func (j *ProvisionJobServer) AddEvent(ev *provisionerpb.ProvisionEvent) {
	event := models.WorkspaceStreamEvent{
		Id:        j.NextEventId,
		Type:      models.WorkspaceStreamEventType(ev.Type),
		Timestamp: ev.Timestamp,
		Message:   ev.Message,
		Status:    models.WorkspacePodStatus(ev.Status),
	}
	j.Events = append(j.Events, event)
	j.NextEventId++
	j.update()
}

func (j *ProvisionJobServer) SetStatus(status models.ProvisionJobStatus) {
	now := time.Now().UTC()
	j.Status = status
	if status == models.ProvisionJobCompleted {
		j.FinishedAt = now
	}
	j.UpdatedAt = now
	j.update()
}

func (j *ProvisionJobServer) update() {
	j.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(j)
	if err != nil {
		j.log.Error().Err(err).Msg("Failed to marshal provision job for KV store")
		return
	}
	_, err = j.kv.Set(j.ID, b)
	if err != nil {
		j.log.Error().Err(err).Msg("Failed to update provision job in KV store")
	}
}

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

// FindWorkspace retrieves the details of a specific workspace
func (p *ProvisionerService) FindWorkspace(ctx context.Context,
	req *provisionerpb.FindWorkspaceRequest) (*commonpb.WorkspaceDetails, error) {
	s, _, err := ws.FindWorkspace(ctx, p.server.helm, req.Workspace)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return nil, status.Errorf(codes.NotFound, "Workspace %s not found", req.Workspace)
		}
		return nil, status.Errorf(codes.Internal, "Failed to get workspace details: %v", err)
	}
	return gapi.WorkspaceDetailsToProto(s), nil
}

// ListWorkspaces lists all workspaces, optionally filtered by user and/or blueprint
func (p *ProvisionerService) GetWorkspaces(
	ctx context.Context,
	req *provisionerpb.GetWorkspacesRequest,
) (*provisionerpb.GetWorkspacesResponse, error) {

	workspaces, err := ws.GetWorkspaces(ctx, p.server.helm,
		ws.GetWorkspacesOptions{
			Username:     req.Username,
			Blueprint:    req.Blueprint,
			Organization: req.Organization,
			Workspace:    req.Workspace,
			RepoName:     req.RepoName,
			RepoOwner:    req.RepoOwner,
			RepoRef:      req.RepoRef,
		})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to list workspaces: %v", err)
	}

	var protoWorkspaces []*commonpb.WorkspaceDetails
	for _, w := range workspaces.Workspaces {
		protoWorkspaces = append(protoWorkspaces, gapi.WorkspaceDetailsToProto(w))
	}

	return &provisionerpb.GetWorkspacesResponse{
		Workspaces: protoWorkspaces,
	}, nil
}

func (p *ProvisionerService) CanUpgradeWorkspace(ctx context.Context,
	req *provisionerpb.CanUpgradeWorkspaceRequest,
) (*provisionerpb.CanUpgradeWorkspaceResponse, error) {
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

	return &provisionerpb.CanUpgradeWorkspaceResponse{
		CanUpgrade: canUpgrade,
		Message:    message,
	}, nil
}

func (p *ProvisionerService) sendProvisionEvent(
	stream provisionerpb.ProvisionerService_ProvisionWorkspaceStreamServer,
	job *ProvisionJobServer,
	event *provisionerpb.ProvisionEvent,
) error {
	err := stream.Send(&provisionerpb.ProvisionWorkspaceResponse{
		Data: &provisionerpb.ProvisionWorkspaceResponse_Event{
			Event: event,
		},
	})
	if err != nil {
		p.log.Error().Err(err).Msg("Failed to send provision event")
	}
	if job != nil {
		job.AddEvent(event)
	}
	return err
}

func (p *ProvisionerService) sendProvisionHandshakeErr(
	stream provisionerpb.ProvisionerService_ProvisionWorkspaceStreamServer,
	workspaceName string, err error,
) error {
	errx := stream.Send(&provisionerpb.ProvisionWorkspaceResponse{
		Data: &provisionerpb.ProvisionWorkspaceResponse_Handshake{
			Handshake: &provisionerpb.HandshakeResponse{
				Workspace: workspaceName,
				Error:     err.Error(),
			},
		},
	})
	if errx != nil {
		p.log.Error().Err(errx).Msg("Failed to send handshake error response")
	}
	return errx
}

func (p *ProvisionerService) waitForWorkspacePodGone(ctx context.Context, name string, timeout time.Duration) error {
	if name == "" {
		return fmt.Errorf("workspace name is required")
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	v1 := p.server.helm.KubeClient().CoreV1()
	ns := p.server.helm.TargetNamespace()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		_, err := v1.Pods(ns).Get(wctx, name, metav1.GetOptions{})
		if err != nil {
			if k8sErrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("failed to get workspace pod %s while waiting for deletion: %w", name, err)
		}

		select {
		case <-wctx.Done():
			return fmt.Errorf("timeout waiting for workspace pod %s to be deleted: %w", name, wctx.Err())
		case <-ticker.C:
		}
	}
}

// ProvisionWorkspaceStream provisions a new workspace with streaming updates
func (p *ProvisionerService) ProvisionWorkspaceStream(
	req *provisionerpb.ProvisionWorkspaceRequest,
	stream provisionerpb.ProvisionerService_ProvisionWorkspaceStreamServer,
) error {

	var job *ProvisionJobServer
	ctx := stream.Context()

	timeout := int(req.Timeout)
	if timeout <= 0 {
		timeout = 20
	}

	userstr, err := models.NewUserStr(req.Userstr, false)
	if err != nil {
		return p.sendProvisionHandshakeErr(stream, "", status.Errorf(codes.InvalidArgument,
			"Invalid userstr format: %v", err))
	}

	canUserStr, err := userstr.Canonicalize()
	if err != nil {
		return p.sendProvisionHandshakeErr(stream, "", status.Errorf(codes.InvalidArgument,
			"Failed to canonicalize userstr: %v", err))
	}

	workspace, err := p.prepareWorkspaceWithUserStr(ctx, canUserStr)
	if err != nil {
		return p.sendProvisionHandshakeErr(stream, "", err)
	}

	exists, st, err := workspace.ExistsAndRunning(ctx)
	if err != nil {
		return p.sendProvisionHandshakeErr(stream, workspace.Name, status.Errorf(codes.Internal,
			"Failed to check if workspace can be provisioned: %v", err))
	}
	if exists {
		return p.sendProvisionHandshakeErr(stream, workspace.Name, status.Errorf(codes.AlreadyExists,
			"Workspace %s already exists and is running", workspace.Name))
	}

	if st != nil {
		if st.Status == models.WorkspaceStatusTerminating || st.Status == models.WorkspaceStatusStopped {
			p.log.Debug().Msgf("Workspace %s is in %s state, waiting for it to be deleted before provisioning",
				workspace.Name, st.Status)
			waitDur := time.Duration(timeout) * time.Second
			if err := p.waitForWorkspacePodGone(ctx, workspace.Name, waitDur); err != nil {
				return p.sendProvisionHandshakeErr(stream, workspace.Name, status.Errorf(codes.DeadlineExceeded,
					"Workspace %s is still being deleted; please retry: %v", workspace.Name, err))
			} else {
				p.log.Debug().Msgf("Workspace %s deletion detected, proceeding with provisioning", workspace.Name)
			}
		}
	}

	if p.server.provisionJobsKV != nil {
		job = NewProvisionJob(workspace.Name, canUserStr.Identity.Username, p.server.provisionJobsKV, p.log)
		workspace.SetJobId(job.ID)
		job.SetStatus(models.ProvisionJobRunning)
	}

	err = stream.Send(&provisionerpb.ProvisionWorkspaceResponse{
		Data: &provisionerpb.ProvisionWorkspaceResponse_Handshake{
			Handshake: &provisionerpb.HandshakeResponse{
				Workspace: workspace.Name,
				Jobid:     workspace.JobId,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to send handshake response: %w", err)
	}

	messages := make(chan models.WorkspaceStreamEvent, 100)
	done := make(chan *models.PodStatus)
	errorChan := make(chan error)
	progress := 0
	percent := 0

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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case msg, ok := <-messages:
			if !ok {
				continue
			}
			if req.SendEvents {
				if err := p.sendProvisionEvent(stream, job, &provisionerpb.ProvisionEvent{
					Type:       string(models.WorkspaceStreamEventTypeEvent),
					Timestamp:  msg.Timestamp,
					ObjectName: msg.ObjectName,
					Message:    msg.Message,
				}); err != nil {
					p.log.Error().Err(err).Msg("Failed to send provision event message")
				}
			}

			if req.SendProgress {
				progress++
				newPerc := min((progress*100)/TOTAL_PROVISION_EVENTS, 100)
				if newPerc > percent {
					percent = newPerc
					if err := p.sendProvisionEvent(stream, job, &provisionerpb.ProvisionEvent{
						Type:       string(models.WorkspaceStreamEventTypeProgress),
						Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
						ObjectName: workspace.Name,
						Status:     fmt.Sprintf("%d", percent),
						Message:    fmt.Sprintf("%d%% complete", percent),
					}); err != nil {
						p.log.Error().Err(err).Msg("Failed to send provision progress event")
					}
				}
			}

		case status := <-done:
			if status != nil {
				if err := p.sendProvisionEvent(stream, job, &provisionerpb.ProvisionEvent{
					Type:       string(models.WorkspaceStreamEventTypeStatus),
					Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
					ObjectName: workspace.Name,
					Status:     string(status.Status),
					Message:    status.Message,
				}); err != nil {
					p.log.Error().Err(err).Msg("Failed to send provision status event")
				}
			}
			if job != nil {
				job.SetStatus(models.ProvisionJobCompleted)
			}
			return nil

		case err := <-errorChan:
			if err != nil {
				if err := p.sendProvisionEvent(stream, job, &provisionerpb.ProvisionEvent{
					Type:       string(models.WorkspaceStreamEventTypeStatus),
					Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
					ObjectName: workspace.Name,
					Status:     string(models.WorkspaceStatusError),
					Message:    err.Error(),
				}); err != nil {
					p.log.Error().Err(err).Msg("Failed to send provision error event")
				}
			}
			if job != nil {
				job.SetStatus(models.ProvisionJobCompleted)
			}
			return nil
		}
	}
}

// prepareWorkspaceName prepares the workspace object for provisioning/upgrade based on the workspace name
func (p *ProvisionerService) prepareWorkspaceWithPod(ctx context.Context, pod *corev1.Pod) (*ws.Workspace, error) {

	userstrb64 := pod.Labels["k8shell.io/userstr"]
	if userstrb64 == "" {
		return nil, status.Errorf(codes.Internal, "Workspace pod is missing userstr label")
	}

	userstr, err := models.NewCanonicalUserStrFromBase64(userstrb64)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid userstr format: %v", err)
	}

	workspace, err := p.prepareWorkspaceWithUserStr(ctx, userstr)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to prepare workspace for upgrade check: %v", err)
	}

	return workspace, nil
}

// prepareWorkspace prepares the workspace object for provisioning/upgrade
// based on the user string and blueprint information
func (p *ProvisionerService) prepareWorkspaceWithUserStr(ctx context.Context,
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

		scope, errx := p.server.GetBlueprintScope(customBlueprint.Metadata.Name, user, &customBlueprint.Metadata, userStr.WorkspaceName)
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

		scope, errx := p.server.GetBlueprintScope(bpName, user, nil, userStr.WorkspaceName)
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

	workspace, err := ws.NewWorkspace(userStr.WorkspaceName, blueprintObj, user, userStr,
		p.server.helm, p.server.Identity, p.server.config)
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

func (p *ProvisionerService) UpgradeWorkspaceResources(ctx context.Context,
	req *provisionerpb.UpgradeWorkspaceResourcesRequest) (*provisionerpb.UpgradeWorkspaceResourcesResponse, error) {

	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	if req.Cpu == "" && req.Memory == "" {
		return nil, status.Errorf(codes.InvalidArgument, "at least one of cpu or memory must be specified")
	}

	_, pod, err := ws.FindWorkspace(ctx, p.server.helm, name)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return nil, status.Errorf(codes.NotFound, "Workspace %s not found", name)
		}
		return nil, status.Errorf(codes.Internal, "Failed to find workspace: %v", err)
	}

	wl, err := ws.ParseWorkspaceLabels(pod.Labels)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to parse workspace labels: %v", err)
	}

	userpb, err := p.server.Identity.FindUser(ctx, &identitypb.FindUserRequest{Username: wl.Username})
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

	return &provisionerpb.UpgradeWorkspaceResourcesResponse{
		Status:  "Success",
		Message: fmt.Sprintf("Workspace %s resources upgraded successfully", name),
	}, nil

}

func (p *ProvisionerService) UpgradeWorkspaceStream(
	req *provisionerpb.UpgradeWorkspaceRequest,
	stream grpc.ServerStreamingServer[provisionerpb.ProvisionWorkspaceResponse],
) error {
	name := req.Workspace
	if name == "" {
		return p.sendProvisionHandshakeErr(stream, "", status.Errorf(codes.InvalidArgument,
			"workspace name is required"))
	}

	ctx := stream.Context()
	_, pod, err := ws.FindWorkspace(ctx, p.server.helm, name)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return p.sendProvisionHandshakeErr(stream, name, status.Errorf(codes.NotFound,
				"Workspace %s not found", name))
		}
		return p.sendProvisionHandshakeErr(stream, name, status.Errorf(codes.Internal,
			"Failed to find workspace: %v", err))
	}

	wl, err := ws.ParseWorkspaceLabels(pod.Labels)
	if err != nil {
		return p.sendProvisionHandshakeErr(stream, name, status.Errorf(codes.Internal,
			"Failed to parse workspace labels: %v", err))
	}

	_, err = p.server.Identity.FindUser(ctx, &identitypb.FindUserRequest{Username: wl.Username})
	if err != nil {
		return p.sendProvisionHandshakeErr(stream, name, status.Errorf(codes.Internal,
			"Failed to get user %s: %v", wl.Username, err))
	}

	if !req.Force {
		workspace, err := p.prepareWorkspaceWithPod(ctx, pod)
		if err != nil {
			return p.sendProvisionHandshakeErr(stream, name, err)
		}

		canUpgrade, err := workspace.CanUpgrade(ctx, pod)
		if err != nil {
			return p.sendProvisionHandshakeErr(stream, name, status.Errorf(codes.Internal,
				"Failed to check if workspace can be upgraded: %v", err))
		}

		if !canUpgrade {
			return p.sendProvisionHandshakeErr(stream, name, status.Errorf(codes.FailedPrecondition,
				"Workspace %s cannot be upgraded because it is already up to date.", name))
		}
	}

	_, err = p.DeleteWorkspace(ctx, &provisionerpb.DeleteWorkspaceRequest{
		Workspace:    name,
		DelaySeconds: 0,
	})
	if err != nil {
		return p.sendProvisionHandshakeErr(stream, name, status.Errorf(codes.Internal,
			"Failed to delete workspace %s for upgrade: %v", name, err))
	}

	// this is temp until we have a better way to coordinate the upgrade process and
	// ensure the workspace is fully deleted before starting provisioning again
	time.Sleep(time.Duration(2) * time.Second)

	return p.ProvisionWorkspaceStream(&provisionerpb.ProvisionWorkspaceRequest{
		Userstr:      wl.UserStr.CanonicalUserStr,
		Timeout:      req.Timeout,
		SendEvents:   req.SendEvents,
		SendProgress: req.SendProgress,
	}, stream)
}

// DeleteWorkspace deletes a workspace asynchronously with distributed locking
func (p *ProvisionerService) DeleteWorkspace(ctx context.Context,
	req *provisionerpb.DeleteWorkspaceRequest) (*provisionerpb.DeleteWorkspaceResponse, error) {

	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	if req.DelaySeconds > 60 {
		return nil, status.Errorf(codes.InvalidArgument, "delay seconds cannot be greater than 60 seconds")
	}

	w, err := ws.NewWorkspaceFromHelmRelease(ctx, name, p.server.helm, p.server.Identity, p.server.config)
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

	if req.DelaySeconds > 0 {
		// asynchronously delete the workspace after the specified delay and release the lock when done
		go func() {
			defer func() {
				unlockCtx := context.Background()
				if unlockErr := workspaceLock.Release(unlockCtx); unlockErr != nil {
					p.log.Error().Err(unlockErr).Msgf("Failed to release lock after deleting workspace %s", name)
				}
			}()

			time.Sleep(time.Duration(req.DelaySeconds) * time.Second)
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

	// synchronously delete the workspace and release the lock when done
	defer func() {
		unlockCtx := context.Background()
		if unlockErr := workspaceLock.Release(unlockCtx); unlockErr != nil {
			p.log.Error().Err(unlockErr).Msgf("Failed to release lock after deleting workspace %s", name)
		}
	}()

	err = w.Uninstall(context.Background(), time.Duration(10)*time.Second, false, false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to delete workspace %s: %v", name, err)
	}

	p.log.Info().Msgf("Successfully deleted workspace %s", name)
	return &provisionerpb.DeleteWorkspaceResponse{
		Message: fmt.Sprintf("Successfully deleted workspace %s", name),
	}, nil
}
