package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/google/uuid"
	commonv1 "github.com/k8shell-io/common/pkg/api/gen/go/common/v1"
	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	natsc "github.com/k8shell-io/common/pkg/nats"
	"github.com/k8shell-io/common/pkg/userstr"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

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

func (j *ProvisionJobServer) AddEvent(ev *provisionerv1.ProvisionEvent) {
	event := models.WorkspaceStreamEvent{
		Id:         j.NextEventId,
		Type:       models.WorkspaceStreamEventType(ev.Type),
		Timestamp:  ev.Timestamp,
		ObjectName: ev.ObjectName,
		Message:    ev.Message,
		Status:     models.WorkspaceStatusMessage(ev.Status),
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
	provisionerv1.UnimplementedProvisionerServiceServer
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
	req *provisionerv1.FindWorkspaceRequest) (*commonv1.WorkspaceDetails, error) {
	s, _, err := ws.FindWorkspace(ctx, p.server.helm, req.Workspace)
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
			Username:     req.Username,
			Blueprint:    req.Blueprint,
			Organization: req.Organization,
			Workspace:    req.Workspace,
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

func (p *ProvisionerService) sendProvisionEvent(
	stream provisionerv1.ProvisionerService_ProvisionWorkspaceStreamServer,
	job *ProvisionJobServer,
	event *provisionerv1.ProvisionEvent,
) error {
	err := stream.Send(&provisionerv1.ProvisionWorkspaceResponse{
		Data: &provisionerv1.ProvisionWorkspaceResponse_Event{
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

// handshakeErrSender abstracts the stream-specific wrapping needed to deliver a
// HandshakeResponse error to the client.
type handshakeErrSender interface {
	sendHandshake(h *provisionerv1.HandshakeResponse) error
}

type provisionHandshakeSender struct {
	s provisionerv1.ProvisionerService_ProvisionWorkspaceStreamServer
}

func (w provisionHandshakeSender) sendHandshake(h *provisionerv1.HandshakeResponse) error {
	return w.s.Send(&provisionerv1.ProvisionWorkspaceResponse{
		Data: &provisionerv1.ProvisionWorkspaceResponse_Handshake{Handshake: h},
	})
}

func (p *ProvisionerService) sendHandshakeErr(sender handshakeErrSender, workspaceName string, handshakeErr error) error {
	errx := sender.sendHandshake(&provisionerv1.HandshakeResponse{
		Workspace: workspaceName,
		Error:     handshakeErr.Error(),
	})
	if errx != nil {
		p.log.Error().Err(errx).Msg("failed to send handshake error")
	}
	return errx
}

// ProvisionWorkspaceStream provisions a new workspace with streaming updates
func (p *ProvisionerService) ProvisionWorkspaceStream(
	req *provisionerv1.ProvisionWorkspaceRequest,
	stream provisionerv1.ProvisionerService_ProvisionWorkspaceStreamServer,
) error {
	var job *ProvisionJobServer
	ctx := stream.Context()
	msgStream := provisionHandshakeSender{stream}

	parsedUserStr, err := userstr.ParseUserStr(req.Userstr)
	if err != nil {
		return p.sendHandshakeErr(msgStream, "", status.Errorf(codes.InvalidArgument,
			"invalid userstr format: %v", err))
	}

	canUserStr, err := parsedUserStr.Canonicalize()
	if err != nil {
		return p.sendHandshakeErr(msgStream, "", status.Errorf(codes.InvalidArgument,
			"failed to canonicalize userstr: %v", err))
	}

	identity := canUserStr.Identity()
	workspaceNamespace := parsedUserStr.Namespace("")
	podName := parsedUserStr.Pod()
	injectMode := workspaceNamespace != "" && podName != ""

	timeout := int(req.Timeout)
	if timeout <= 0 {
		if injectMode {
			timeout = 120
		} else {
			timeout = 20
		}
	}

	workspace, err := p.prepareWorkspaceWithUserStr(ctx, canUserStr)
	if err != nil {
		return p.sendHandshakeErr(msgStream, "", err)
	}

	tokenResp, err := p.server.Identity.GetUserAccessToken(ctx, &identityv1.GetUserAccessTokenRequest{
		Username: identity.Username(),
	})
	if err != nil {
		return p.sendHandshakeErr(msgStream, workspace.Name, status.Errorf(codes.Unauthenticated,
			"failed to retrieve identity token for user %s: %v", identity.Username(), err))
	}
	if _, err := p.server.tokenVerifier.VerifyToken(tokenResp.AccessToken); err != nil {
		return p.sendHandshakeErr(msgStream, workspace.Name, status.Errorf(codes.Unauthenticated,
			"identity token for user %s is invalid: %v", identity.Username(), err))
	}
	workspace.SetIdentityToken(tokenResp.AccessToken)

	deploymentName := ""
	if injectMode {
		deploymentName, err = p.resolveDeploymentNameFromPod(ctx, workspaceNamespace, podName)
		if err != nil {
			return p.sendHandshakeErr(msgStream, workspace.Name, err)
		}
	} else {
		exists, st, err := workspace.ExistsAndRunning(ctx)
		if err != nil {
			return p.sendHandshakeErr(msgStream, workspace.Name, status.Errorf(codes.Internal,
				"Failed to check if workspace can be provisioned: %v", err))
		}
		if exists {
			return p.sendHandshakeErr(msgStream, workspace.Name, status.Errorf(codes.AlreadyExists,
				"Workspace %s already exists and is running", workspace.Name))
		}

		if st != nil && st.Status == models.WorkspaceStatusTerminating {
			p.log.Debug().Msgf("Workspace %s is terminating, waiting for pod to be gone", workspace.Name)
			waitDur := time.Duration(timeout) * time.Second
			if err := p.waitForWorkspacePodGone(ctx, workspace.Name, waitDur); err != nil {
				return p.sendHandshakeErr(msgStream, workspace.Name, status.Errorf(codes.DeadlineExceeded,
					"Workspace %s is still being deleted; please retry: %v", workspace.Name, err))
			}
			p.log.Debug().Msgf("Workspace %s deletion detected, proceeding with provisioning", workspace.Name)
		}

	}

	if p.server.provisionJobsKV != nil {
		job = NewProvisionJob(workspace.Name, identity.Username(), p.server.provisionJobsKV, p.log)
		workspace.SetJobId(job.ID)
		job.SetStatus(models.ProvisionJobRunning)
	}

	if err := stream.Send(&provisionerv1.ProvisionWorkspaceResponse{
		Data: &provisionerv1.ProvisionWorkspaceResponse_Handshake{Handshake: &provisionerv1.HandshakeResponse{
			Workspace: workspace.Name,
			Jobid:     workspace.JobId,
		}},
	}); err != nil {
		return fmt.Errorf("failed to send handshake response: %w", err)
	}

	messages := make(chan models.WorkspaceStreamEvent, 100)
	done := make(chan *models.WorkspaceStatus)
	errorChan := make(chan error)
	provisioningMilestones := []int{20, 40, 60, 75}
	provisioningStep := 0
	progressPct := 0

	go func() {
		defer close(done)
		defer close(errorChan)

		var st *models.WorkspaceStatus
		var runErr error
		if injectMode {
			st, runErr = workspace.Inject(ctx, &ws.InjectOptions{
				Namespace:            workspaceNamespace,
				DeploymentName:       deploymentName,
				WorkspaceCanonicalId: canUserStr.CanonicalId(),
				Timeout:              timeout,
				Messages:             messages,
			})
		} else {
			st, runErr = workspace.Provision(ctx, &ws.ProvisionOptions{
				Timeout:  timeout,
				Messages: messages,
			})
		}

		if runErr != nil {
			errorChan <- runErr
			return
		}
		done <- st
	}()

	emitEvent := func(ev *provisionerv1.ProvisionEvent) {
		if err := p.sendProvisionEvent(stream, job, ev); err != nil {
			p.log.Error().Err(err).Msg("failed to send stream event")
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case msg, ok := <-messages:
			if !ok {
				continue
			}

			if msg.Type == models.WorkspaceStreamEventTypeStatus {
				emitEvent(&provisionerv1.ProvisionEvent{
					Type:       string(models.WorkspaceStreamEventTypeStatus),
					Timestamp:  msg.Timestamp,
					ObjectName: msg.ObjectName,
					Status:     string(msg.Status),
					Message:    msg.Message,
				})
			}

			if req.SendEvents && msg.Type == models.WorkspaceStreamEventTypeEvent {
				emitEvent(&provisionerv1.ProvisionEvent{
					Type:       string(models.WorkspaceStreamEventTypeEvent),
					Timestamp:  msg.Timestamp,
					ObjectName: msg.ObjectName,
					Message:    msg.Message,
				})
			}

			if req.SendProgress && msg.Type == models.WorkspaceStreamEventTypeStatus {
				var newPerc int
				switch msg.Status {
				case models.WorkspaceStatusProvisioning:
					if provisioningStep < len(provisioningMilestones) {
						newPerc = provisioningMilestones[provisioningStep]
						provisioningStep++
					}
				case models.WorkspaceStatusPulling:
					newPerc = 50
				case models.WorkspaceStatusRunning:
					newPerc = 100
				}
				if newPerc > progressPct {
					progressPct = newPerc
					emitEvent(&provisionerv1.ProvisionEvent{
						Type:       string(models.WorkspaceStreamEventTypeProgress),
						Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
						ObjectName: workspace.Name,
						Status:     fmt.Sprintf("%d", progressPct),
						Message:    fmt.Sprintf("%d%% complete", progressPct),
					})
				}
			}

		case st := <-done:
			finalStatus := models.WorkspaceStatusUnknown
			finalMessage := ""
			if st != nil {
				finalStatus = st.Status
				finalMessage = st.Message
			}

			emitEvent(&provisionerv1.ProvisionEvent{
				Type:       string(models.WorkspaceStreamEventTypeStatus),
				Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
				ObjectName: workspace.Name,
				Status:     string(finalStatus),
				Message:    finalMessage,
			})

			if req.SendProgress && progressPct < 100 {
				emitEvent(&provisionerv1.ProvisionEvent{
					Type:       string(models.WorkspaceStreamEventTypeProgress),
					Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
					ObjectName: workspace.Name,
					Status:     "100",
					Message:    "100% complete",
				})
			}

			if job != nil {
				job.SetStatus(models.ProvisionJobCompleted)
			}
			return nil

		case runErr := <-errorChan:
			if runErr != nil {
				emitEvent(&provisionerv1.ProvisionEvent{
					Type:       string(models.WorkspaceStreamEventTypeStatus),
					Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
					ObjectName: workspace.Name,
					Status:     string(models.WorkspaceStatusError),
					Message:    runErr.Error(),
				})
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

	userstrb64 := pod.Annotations["k8shell.io/userstr"]
	if userstrb64 == "" {
		return nil, status.Errorf(codes.Internal, "Workspace pod is missing userstr annotation")
	}

	parsedUserStr, err := userstr.ParseUserStr("b64-" + userstrb64)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid userstr format: %v", err)
	}

	canonicalUserStr, err := parsedUserStr.Canonicalize()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid userstr format: %v", err)
	}

	workspace, err := p.prepareWorkspaceWithUserStr(ctx, canonicalUserStr)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to prepare workspace for upgrade check: %v", err)
	}

	return workspace, nil
}

// prepareWorkspace prepares the workspace object for provisioning/upgrade
// based on the user string and blueprint information
func (p *ProvisionerService) prepareWorkspaceWithUserStr(ctx context.Context,
	userStr *userstr.CanonicalUserStr) (*ws.Workspace, error) {

	identity := userStr.Identity()
	canonicalUserStr := userStr.CanonicalUserStr()
	workspaceName := userStr.WorkspaceName()

	userpb, err := p.server.Identity.FindUser(ctx, &identityv1.FindUserRequest{Username: identity.Username()})
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Failed to get user: %v", err)
	}
	user := gapi.ProtoToUser(userpb)

	var blueprintObj *models.Blueprint
	var resolvedBpName string
	switch {
	case identity.BlueprintKind() == userstr.BlueprintKindCustom:
		blueprintpb, err := p.server.Identity.GetBlueprintByUserStr(ctx, &identityv1.UserStr{Userstr: canonicalUserStr})
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to get blueprint by userstr: %v", err)
		}

		p.log.Debug().Str("userstr", canonicalUserStr).Str("blueprint",
			string(blueprintpb.Blueprint)).Msg("Retrieved k8shell file for userstr")

		var parsedCustomBlueprint *models.CustomBlueprint
		useDefault := false

		if len(blueprintpb.Blueprint) == 0 {
			p.log.Info().Str("userstr", canonicalUserStr).
				Msg("custom blueprint not found (empty response); falling back to default custom blueprint")
			useDefault = true
		} else {
			var k8shellFile models.K8shellFile
			if err := yaml.Unmarshal(blueprintpb.Blueprint, &k8shellFile); err != nil {
				p.log.Warn().Str("userstr", canonicalUserStr).Err(err).
					Msg("failed to parse k8shell file; falling back to default custom blueprint")
				useDefault = true
			} else {
				customBp, validationErrors := models.ValidateK8shellFile(k8shellFile)
				if len(validationErrors) > 0 {
					p.log.Warn().Str("userstr", canonicalUserStr).
						Strs("errors", validationErrors).
						Msg("k8shell file validation failed; falling back to default custom blueprint")
					useDefault = true
				} else {
					parsedCustomBlueprint = customBp
				}
			}
		}

		p.log.Debug().Str("userstr", canonicalUserStr).Bool("useDefault", useDefault).
			Msg("Custom blueprint parsing result")

		if useDefault {
			defaultBp := p.server.config.Blueprints.DefaultCustomBlueprint
			if defaultBp == "" {
				return nil, status.Errorf(codes.NotFound,
					"custom blueprint not found for userstr %s and no defaultCustomBlueprint is configured",
					canonicalUserStr)
			}
			p.log.Info().Str("userstr", canonicalUserStr).Str("default", defaultBp).
				Msg("using default custom blueprint")

			defaultBpMetadata := &models.BlueprintMetadata{
				Name:        identity.Blueprint(),
				RepoName:    identity.RepoName(),
				RepoOwner:   identity.RepoOwner(),
				RepoRef:     identity.RepoRef(),
				RepoAddress: blueprintpb.RepoAddress,
			}
			scope, errx := p.server.GetBlueprintScope(defaultBp, user, defaultBpMetadata, workspaceName)
			if errx != nil {
				return nil, convertToGRPCError(errx)
			}
			if !user.HasBlueprint(defaultBp) {
				return nil, status.Errorf(codes.PermissionDenied,
					"Access denied: user %s is not authorized to use default blueprint %s",
					identity.Username(), defaultBp)
			}
			blueprintObj, err = p.server.bpManager.GetBlueprint(defaultBp, scope)
			if err != nil {
				return nil, status.Errorf(codes.NotFound, "default custom blueprint %q not found: %v", defaultBp, err)
			}
			resolvedBpName = defaultBp
			user = scope.User
			break
		}

		parsedCustomBlueprint.Name = identity.Blueprint()
		parsedCustomBlueprint.Metadata.Name = identity.Blueprint()
		parsedCustomBlueprint.Metadata.RepoName = identity.RepoName()
		parsedCustomBlueprint.Metadata.RepoOwner = identity.RepoOwner()
		parsedCustomBlueprint.Metadata.RepoRef = identity.RepoRef()
		parsedCustomBlueprint.Metadata.RepoAddress = blueprintpb.RepoAddress

		if !user.HasBlueprint(parsedCustomBlueprint.Template) {
			return nil, status.Errorf(codes.PermissionDenied,
				"Access denied: user %s is not authorized to use blueprint's template %s", identity.Username(), parsedCustomBlueprint.Template)
		}

		scope, errx := p.server.GetBlueprintScope(parsedCustomBlueprint.Metadata.Name, user, &parsedCustomBlueprint.Metadata, workspaceName)
		if errx != nil {
			return nil, convertToGRPCError(errx)
		}

		blueprintObj, err = p.server.bpManager.ComposeWithScope(parsedCustomBlueprint, scope)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to compose blueprint: %v", err)
		}

		user = scope.User

	case identity.BlueprintKind() == userstr.BlueprintKindImplicit || identity.BlueprintKind() == userstr.BlueprintKindExplicit:
		bpName := identity.Blueprint()
		if identity.BlueprintKind() == userstr.BlueprintKindImplicit {
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

		scope, errx := p.server.GetBlueprintScope(bpName, user, nil, workspaceName)
		if errx != nil {
			return nil, convertToGRPCError(errx)
		}

		if !user.HasBlueprint(bpName) {
			return nil, status.Errorf(codes.PermissionDenied,
				"Access denied: user %s is not authorized to use blueprint %s", identity.Username(), bpName)
		}

		blueprintObj, err = p.server.bpManager.GetBlueprint(bpName, scope)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "Blueprint %s not found", identity.Blueprint())
		}

		if blueprintObj.IsTemplate {
			return nil, status.Errorf(codes.InvalidArgument,
				"Blueprint %s is a template and cannot be used to provision a workspace", identity.Blueprint())
		}
		resolvedBpName = bpName
	}

	workspace, err := ws.NewWorkspace(workspaceName, blueprintObj, user, userStr,
		p.server.helm, p.server.Identity, p.server.config)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create workspace: %v", err)
	}
	workspace.SetBlueprintChain(p.server.bpManager.GetBlueprintChain(resolvedBpName))

	return workspace, nil
}

func (p *ProvisionerService) UpgradeWorkspaceResources(ctx context.Context,
	req *provisionerv1.UpgradeWorkspaceResourcesRequest) (*provisionerv1.UpgradeWorkspaceResourcesResponse, error) {

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
	_, pod, err := ws.FindWorkspace(ctx, p.server.helm, name)
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

// DeleteWorkspace deletes a workspace asynchronously with distributed locking
func (p *ProvisionerService) DeleteWorkspace(ctx context.Context,
	req *provisionerv1.DeleteWorkspaceRequest) (*provisionerv1.DeleteWorkspaceResponse, error) {

	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	if req.DelaySeconds > 60 {
		return nil, status.Errorf(codes.InvalidArgument, "delay seconds cannot be greater than 60 seconds")
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
			return &provisionerv1.DeleteWorkspaceResponse{
				Message: fmt.Sprintf("Request to delete the workspace %s already exists", name),
			}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"Failed to acquire lock for workspace %s deletion: %v", name, err)
	}

	if !acquired {
		return &provisionerv1.DeleteWorkspaceResponse{
			Message: fmt.Sprintf("Request to delete the workspace %s already exists", name),
		}, nil
	}

	if req.DelaySeconds > 0 {
		// asynchronously delete the workspace after the specified delay and release the lock when done
		// Use WithoutCancel so the goroutine outlives the request context.
		bgCtx := context.WithoutCancel(ctx)
		go func() {
			defer func() {
				if unlockErr := workspaceLock.Release(bgCtx); unlockErr != nil {
					p.log.Error().Err(unlockErr).Msgf("Failed to release lock after deleting workspace %s", name)
				}
			}()

			time.Sleep(time.Duration(req.DelaySeconds) * time.Second)
			p.log.Debug().Msgf("Starting async deletion of workspace %s", name)

			err := w.Uninstall(bgCtx, time.Duration(10)*time.Second, false, false)
			if err != nil {
				p.log.Error().Err(err).Msgf("Failed to delete workspace %s", name)
			} else {
				p.log.Info().Msgf("Successfully deleted workspace %s", name)
			}
		}()

		return &provisionerv1.DeleteWorkspaceResponse{
			Message: fmt.Sprintf("Request to delete the workspace %s was submitted", name),
		}, nil
	}

	// synchronously delete the workspace and release the lock when done
	bgCtx := context.WithoutCancel(ctx)
	defer func() {
		if unlockErr := workspaceLock.Release(bgCtx); unlockErr != nil {
			p.log.Error().Err(unlockErr).Msgf("Failed to release lock after deleting workspace %s", name)
		}
	}()

	err = w.Uninstall(bgCtx, time.Duration(10)*time.Second, false, false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to delete workspace %s: %v", name, err)
	}

	p.log.Info().Msgf("Successfully deleted workspace %s", name)
	return &provisionerv1.DeleteWorkspaceResponse{
		Message: fmt.Sprintf("Successfully deleted workspace %s", name),
	}, nil
}

// StopWorkspace deletes only the workspace pod, leaving the Helm release and all
// other resources (PVCs, secrets, ConfigMaps) intact for later re-provisioning.
func (p *ProvisionerService) StopWorkspace(ctx context.Context,
	req *provisionerv1.StopWorkspaceRequest) (*provisionerv1.StopWorkspaceResponse, error) {

	name := req.Workspace
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}

	if req.DelaySeconds > 60 {
		return nil, status.Errorf(codes.InvalidArgument, "delay seconds cannot be greater than 60 seconds")
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
			return &provisionerv1.StopWorkspaceResponse{
				Message: fmt.Sprintf("Request to stop the workspace %s already exists", name),
			}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"Failed to acquire lock for workspace %s: %v", name, err)
	}

	if !acquired {
		return &provisionerv1.StopWorkspaceResponse{
			Message: fmt.Sprintf("Request to stop the workspace %s already exists", name),
		}, nil
	}

	if req.DelaySeconds > 0 {
		bgCtx := context.WithoutCancel(ctx)
		go func() {
			defer func() {
				if unlockErr := workspaceLock.Release(bgCtx); unlockErr != nil {
					p.log.Error().Err(unlockErr).Msgf("Failed to release lock after stopping workspace %s", name)
				}
			}()

			time.Sleep(time.Duration(req.DelaySeconds) * time.Second)
			p.log.Debug().Msgf("Starting async stop of workspace pod %s", name)

			if err := w.StopPod(bgCtx); err != nil {
				p.log.Error().Err(err).Msgf("Failed to stop workspace pod %s", name)
			} else {
				p.log.Info().Msgf("Successfully stopped workspace pod %s", name)
			}
		}()

		return &provisionerv1.StopWorkspaceResponse{
			Message: fmt.Sprintf("Request to stop the workspace %s was submitted", name),
		}, nil
	}

	bgCtx := context.WithoutCancel(ctx)
	defer func() {
		if unlockErr := workspaceLock.Release(bgCtx); unlockErr != nil {
			p.log.Error().Err(unlockErr).Msgf("Failed to release lock after stopping workspace %s", name)
		}
	}()

	if err := w.StopPod(bgCtx); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to stop workspace pod %s: %v", name, err)
	}

	p.log.Info().Msgf("Successfully stopped workspace pod %s", name)
	return &provisionerv1.StopWorkspaceResponse{
		Message: fmt.Sprintf("Successfully stopped workspace %s", name),
	}, nil
}

// *** helpers

// convertToGRPCError converts internal errors to gRPC status errors
func convertToGRPCError(err error) error {
	if errors.Is(err, models.ErrWorkspaceNotFound) {
		return status.Errorf(codes.NotFound, "%s", err.Error())
	}
	if errors.Is(err, models.ErrInvalidParameters) {
		return status.Errorf(codes.InvalidArgument, "%s", err.Error())
	}

	return status.Errorf(codes.Internal, "%s", err.Error())
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

// resolveDeploymentNameFromPod attempts to find the owning Deployment of a Pod by traversing its owner reference chain.
func (p *ProvisionerService) resolveDeploymentNameFromPod(ctx context.Context, namespace, podName string) (string, error) {
	pod, err := p.server.helm.KubeClient().CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			return "", status.Errorf(codes.NotFound, "pod %s/%s not found", namespace, podName)
		}
		return "", status.Errorf(codes.Internal, "failed to get pod %s/%s: %v", namespace, podName, err)
	}

	if deploymentName := deploymentNameFromOwnerChain(ctx, namespace, pod.OwnerReferences,
		p.server.helm.KubeClient()); deploymentName != "" {
		return deploymentName, nil
	}

	return "", status.Errorf(codes.NotFound, "could not determine deployment for pod %s/%s", namespace, podName)
}

// deploymentNameFromOwnerChain traverses the owner reference chain of a pod to find the owning Deployment, if it exists.
func deploymentNameFromOwnerChain(ctx context.Context, namespace string, owners []metav1.OwnerReference,
	kubeClient kubernetes.Interface) string {
	for _, owner := range owners {
		switch owner.Kind {
		case "Deployment":
			return owner.Name
		case "ReplicaSet":
			rs, err := kubeClient.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			for _, rsOwner := range rs.OwnerReferences {
				if rsOwner.Kind == "Deployment" {
					return rsOwner.Name
				}
			}
		}
	}
	return ""
}

// EjectWorkspace removes a previously injected workspace from a Deployment and
// deletes all supporting resources (ConfigMaps, PVCs, NetworkPolicies) in the
// target namespace.
func (p *ProvisionerService) EjectWorkspace(
	ctx context.Context,
	req *provisionerv1.EjectWorkspaceRequest,
) (*provisionerv1.EjectWorkspaceResponse, error) {
	if req.Namespace == "" {
		return nil, status.Errorf(codes.InvalidArgument, "namespace is required")
	}
	if req.DeploymentName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "deployment_name is required")
	}

	userstr, err := userstr.ParseUserStr(req.Userstr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid userstr format: %v", err)
	}

	canUserStr, err := userstr.Canonicalize()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to canonicalize userstr: %v", err)
	}

	workspace, err := p.prepareWorkspaceWithUserStr(ctx, canUserStr)
	if err != nil {
		return nil, err
	}

	timeout := int(req.TimeoutSeconds)
	if timeout <= 0 {
		timeout = 60
	}

	if err := workspace.Eject(ctx, &ws.EjectOptions{
		Namespace:      req.Namespace,
		DeploymentName: req.DeploymentName,
		Timeout:        timeout,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to eject workspace %s: %v", workspace.Name, err)
	}

	return &provisionerv1.EjectWorkspaceResponse{
		Workspace: workspace.Name,
	}, nil
}
