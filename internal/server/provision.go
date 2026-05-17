package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/google/uuid"
	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	natsc "github.com/k8shell-io/common/pkg/nats"
	"github.com/k8shell-io/common/pkg/userstr"
	"github.com/k8shell-io/provisioner/internal/helm"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
)

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

	expectedWorkspaceName := canUserStr.WorkspaceName()

	if parsedUserStr.Pod() != "" {
		return p.sendHandshakeErr(msgStream, expectedWorkspaceName, status.Errorf(codes.InvalidArgument,
			"cannot provision workspace using pod name"))
	}

	identity := canUserStr.Identity()
	workspaceNamespace := parsedUserStr.Namespace("")
	workloadName := parsedUserStr.WorkloadName()
	workloadKind := parsedUserStr.WorkloadKind()
	injectMode := workspaceNamespace != "" && workloadName != ""

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
		return p.sendHandshakeErr(msgStream, expectedWorkspaceName, err)
	}

	workloadTargetName := ""
	if injectMode {
		if !p.server.config.AllowsInjectionNamespace(workspaceNamespace) {
			return p.sendHandshakeErr(msgStream, workspace.Name, status.Errorf(codes.PermissionDenied,
				"namespace %s is not allowed for injection", workspaceNamespace))
		}
		if err := p.verifyWorkloadExists(ctx, workspaceNamespace, workloadKind, workloadName); err != nil {
			return p.sendHandshakeErr(msgStream, workspace.Name, err)
		}
		workloadTargetName = workloadName
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
				WorkloadName:         workloadTargetName,
				WorkloadKind:         workloadKind,
				WorkspaceCanonicalId: canUserStr.CanonicalId(),
				JobId:                job.ID,
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

// prepareWorkspaceWithPod prepares the workspace object for provisioning based on the workspace name
func (p *ProvisionerService) prepareWorkspaceWithPod(ctx context.Context, pod *corev1.Pod) (*ws.Workspace, error) {

	userstrb64 := pod.Annotations[helm.AnnotationUserStr]
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

// prepareWorkspaceWithUserStr prepares the workspace object for provisioning/upgrade
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
