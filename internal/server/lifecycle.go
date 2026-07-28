// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
)

// userWorkspaceDeleteLockRenewInterval controls how often DeleteUserWorkspaces
// renews its per-user lock while walking a potentially long list of
// workspaces. It must be well under the lock's lease duration (30s) so a
// renewal has multiple chances to succeed before the lease could expire.
const userWorkspaceDeleteLockRenewInterval = 10 * time.Second

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

	_, pod, findErr := ws.FindWorkspace(ctx, p.server.helm, name, p.server.config.InjectNamespaces)
	if findErr == nil && pod.Labels[helm.LabelInjected] == "true" {
		owner, err := ws.FindOwnerWorkload(ctx, p.server.helm.KubeClient(), pod)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to find owning workload for injected workspace %s: %v", name, err)
		}
		if owner == nil {
			return nil, status.Errorf(codes.Internal,
				"could not determine owning workload for injected workspace %s", name)
		}
		canonicalId := pod.Labels[helm.LabelCanonicalId]
		if canonicalId == "" {
			return nil, status.Errorf(codes.Internal,
				"injected workspace pod for %s is missing canonical-id label", name)
		}
		workspace, err := ws.NewWorkspaceForEject(canonicalId, p.server.helm)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to prepare workspace %s for eject: %v", canonicalId, err)
		}
		ejectOpts := &ws.EjectOptions{
			Namespace:    pod.Namespace,
			WorkloadName: owner.Name,
			WorkloadKind: owner.Kind,
			Timeout:      60,
		}
		if req.DelaySeconds > 0 {
			bgCtx := context.WithoutCancel(ctx)
			go func() {
				time.Sleep(time.Duration(req.DelaySeconds) * time.Second)
				p.log.Debug().Msgf("Starting async eject of workspace %s", name)
				if delErr := ws.DeleteWorkspacePATFromLabels(bgCtx, p.server.Identity, pod.Labels); delErr != nil {
					p.log.Error().Err(delErr).Msgf("Failed to delete PAT for workspace %s", name)
				}
				if ejectErr := workspace.Eject(bgCtx, ejectOpts); ejectErr != nil {
					p.log.Error().Err(ejectErr).Msgf("Failed to eject workspace %s", name)
				} else {
					p.log.Info().Msgf("Successfully ejected workspace %s", name)
				}
			}()
			return &provisionerv1.DeleteWorkspaceResponse{
				Message: fmt.Sprintf("Request to eject the workspace %s was submitted", name),
			}, nil
		}
		if delErr := ws.DeleteWorkspacePATFromLabels(ctx, p.server.Identity, pod.Labels); delErr != nil {
			p.log.Error().Err(delErr).Msgf("Failed to delete PAT for workspace %s", name)
		}
		if err := workspace.Eject(ctx, ejectOpts); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to eject workspace %s: %v", name, err)
		}
		p.log.Info().Msgf("Successfully ejected workspace %s", name)
		return &provisionerv1.DeleteWorkspaceResponse{
			Message: fmt.Sprintf("Successfully ejected workspace %s", name),
		}, nil
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

// DeleteUserWorkspaces hard-deletes every workspace (standalone and injected)
// owned by a user. It is intended as a cleanup step when a user is removed
// from the system: the actual deletion runs in the background so this RPC
// can return quickly to a synchronous caller, but a distributed lock keyed
// by username ensures only one replica performs the cleanup at a time and
// that a call made while cleanup is already in progress is rejected rather
// than starting a second, overlapping run.
func (p *ProvisionerService) DeleteUserWorkspaces(ctx context.Context,
	req *provisionerv1.DeleteUserWorkspacesRequest) (*provisionerv1.DeleteUserWorkspacesResponse, error) {

	username := req.Username
	if username == "" {
		return nil, status.Errorf(codes.InvalidArgument, "username is required")
	}

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	userLock := ws.NewWorkspaceLock(p.server.helm.KubeClient(), p.server.helm.TargetNamespace(), "user-delete-"+username)
	acquired, err := userLock.TryAcquire(lockCtx)
	if err != nil {
		if errors.Is(err, ws.ErrLockAlreadyHeld) {
			return &provisionerv1.DeleteUserWorkspacesResponse{
				Message: fmt.Sprintf("Deletion of workspaces for user %s is already in progress", username),
			}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"Failed to acquire lock for user %s workspace deletion: %v", username, err)
	}
	if !acquired {
		return &provisionerv1.DeleteUserWorkspacesResponse{
			Message: fmt.Sprintf("Deletion of workspaces for user %s is already in progress", username),
		}, nil
	}

	result, err := ws.GetWorkspaces(ctx, p.server.helm, ws.GetWorkspacesOptions{
		Usernames:        []string{username},
		InjectNamespaces: p.server.config.InjectNamespaces,
	})
	if err != nil {
		if releaseErr := userLock.Release(context.WithoutCancel(ctx)); releaseErr != nil {
			p.log.Error().Err(releaseErr).Msgf("Failed to release user-delete lock for %s", username)
		}
		return nil, status.Errorf(codes.Internal, "Failed to list workspaces for user %s: %v", username, err)
	}

	if len(result.Workspaces) == 0 {
		if releaseErr := userLock.Release(context.WithoutCancel(ctx)); releaseErr != nil {
			p.log.Error().Err(releaseErr).Msgf("Failed to release user-delete lock for %s", username)
		}
		return &provisionerv1.DeleteUserWorkspacesResponse{
			Message: fmt.Sprintf("No workspaces found for user %s", username),
		}, nil
	}

	names := make([]string, 0, len(result.Workspaces))
	for _, w := range result.Workspaces {
		names = append(names, w.Name)
	}

	bgCtx := context.WithoutCancel(ctx)
	go p.deleteUserWorkspaces(bgCtx, username, userLock, result.Pods)

	return &provisionerv1.DeleteUserWorkspacesResponse{
		Message:    fmt.Sprintf("Request to delete %d workspace(s) for user %s was submitted", len(names), username),
		Workspaces: names,
	}, nil
}

// deleteUserWorkspaces hard-deletes each pod's workspace in turn and releases
// userLock when finished. It runs in the background after DeleteUserWorkspaces
// has already responded to the caller. While it runs, userLock is
// periodically renewed; if renewal ever fails it means another replica may
// have taken over the same cleanup (the lease expired before this replica
// finished), so remaining deletions are abandoned rather than risk two
// replicas racing through the same user's workspaces at once. Individual
// workspace failures - including one already being deleted by a concurrent
// single-workspace DeleteWorkspace call - are logged and skipped so one bad
// workspace does not block the rest of the batch.
func (p *ProvisionerService) deleteUserWorkspaces(ctx context.Context, username string,
	userLock *ws.WorkspaceLock, pods []corev1.Pod) {

	keepAliveCtx, stopKeepAlive := context.WithCancel(ctx)
	defer stopKeepAlive()
	lockLost := userLock.KeepAlive(keepAliveCtx, userWorkspaceDeleteLockRenewInterval)

	defer func() {
		if err := userLock.Release(context.WithoutCancel(ctx)); err != nil {
			p.log.Error().Err(err).Msgf("Failed to release user-delete lock for %s", username)
		}
	}()

	for i := range pods {
		select {
		case err := <-lockLost:
			p.log.Error().Err(err).Msgf(
				"Lost deletion lock for user %s partway through cleanup; aborting remaining workspaces", username)
			return
		default:
		}

		pod := &pods[i]
		name := pod.Name
		if err := ws.DeleteWorkspacePod(ctx, p.server.helm, p.server.Identity, pod); err != nil {
			if errors.Is(err, ws.ErrLockAlreadyHeld) {
				p.log.Warn().Msgf("Workspace %s for user %s is already being deleted, skipping", name, username)
				continue
			}
			p.log.Error().Err(err).Msgf("Failed to delete workspace %s for user %s", name, username)
			continue
		}
		p.log.Info().Msgf("Successfully deleted workspace %s for user %s", name, username)
	}

	p.log.Info().Msgf("Finished deleting workspaces for user %s", username)
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

	if _, pod, findErr := ws.FindWorkspace(ctx, p.server.helm, name, p.server.config.InjectNamespaces); findErr == nil && pod.Labels[helm.LabelInjected] == "true" {
		return nil, status.Errorf(codes.FailedPrecondition, "workspace %s is injected into a deployment and cannot be stopped", name)
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

			if err := w.DeletePAT(bgCtx); err != nil {
				p.log.Error().Err(err).Msgf("Failed to delete PAT for workspace %s", name)
			}

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

	if err := w.DeletePAT(bgCtx); err != nil {
		p.log.Error().Err(err).Msgf("Failed to delete PAT for workspace %s", name)
	}

	if err := w.StopPod(bgCtx); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to stop workspace pod %s: %v", name, err)
	}

	p.log.Info().Msgf("Successfully stopped workspace pod %s", name)
	return &provisionerv1.StopWorkspaceResponse{
		Message: fmt.Sprintf("Successfully stopped workspace %s", name),
	}, nil
}

// StartWorkspaceStream resumes a previously stopped workspace: it mints a
// fresh PAT (the previous one was deleted on stop) and recreates the pod from
// the existing Helm release, streaming progress the same way
// ProvisionWorkspaceStream does. Calling it on an already-running workspace
// is rejected with AlreadyExists, mirroring ProvisionWorkspaceStream.
func (p *ProvisionerService) StartWorkspaceStream(
	req *provisionerv1.StartWorkspaceRequest,
	stream provisionerv1.ProvisionerService_StartWorkspaceStreamServer,
) error {
	ctx := stream.Context()
	msgStream := provisionHandshakeSender{stream}

	name := req.Workspace
	if name == "" {
		return p.sendHandshakeErr(msgStream, "n/a", status.Errorf(codes.InvalidArgument, "workspace name is required"))
	}

	if _, pod, findErr := ws.FindWorkspace(ctx, p.server.helm, name, p.server.config.InjectNamespaces); findErr == nil && pod.Labels[helm.LabelInjected] == "true" {
		return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.FailedPrecondition,
			"workspace %s is injected into a deployment and cannot be started", name))
	}

	workspace, err := ws.NewWorkspaceFromHelmRelease(ctx, name, p.server.helm, p.server.Identity, p.server.config)
	if err != nil {
		return p.sendHandshakeErr(msgStream, name, convertToGRPCError(err))
	}

	exists, _, err := workspace.ExistsAndRunning(ctx)
	if err != nil {
		return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.Internal,
			"Failed to check if workspace %s can be started: %v", name, err))
	}
	if exists {
		return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.AlreadyExists,
			"Workspace %s already exists and is running", name))
	}

	if _, err := workspace.MintPAT(ctx, PAT_SCOPES); err != nil {
		return p.sendHandshakeErr(msgStream, name, status.Errorf(codes.Internal,
			"Failed to create PAT for workspace %s: %v", name, err))
	}

	timeout := int(req.Timeout)
	if timeout <= 0 {
		timeout = 20
	}

	var job *ProvisionJobServer
	if p.server.provisionJobsKV != nil {
		if workspace.JobId != "" {
			if existing, loadErr := LoadProvisionJob(p.server.provisionJobsKV, workspace.JobId, p.log); loadErr == nil {
				job = existing
			} else {
				p.log.Debug().Err(loadErr).Msgf(
					"No reusable provision job %s for workspace %s, creating a new one", workspace.JobId, name)
			}
		}
		if job == nil {
			job = NewProvisionJob(workspace.Name, workspace.Username(), p.server.provisionJobsKV, p.log)
			workspace.SetJobId(job.ID)
		}
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

	return p.runWorkspaceStream(ctx, stream, workspace.Name, job, req.SendProgress, req.SendEvents,
		func(messages chan models.WorkspaceStreamEvent) (*models.WorkspaceStatus, error) {
			return workspace.Provision(ctx, &ws.ProvisionOptions{
				Timeout:  timeout,
				Messages: messages,
			})
		})
}

// EjectWorkspace removes a previously injected workspace from a workload and
// deletes all supporting resources (ConfigMaps, PVCs, NetworkPolicies) in the
// target namespace.
func (p *ProvisionerService) EjectWorkspace(
	ctx context.Context,
	req *provisionerv1.EjectWorkspaceRequest,
) (*provisionerv1.EjectWorkspaceResponse, error) {
	if req.Namespace == "" {
		return nil, status.Errorf(codes.InvalidArgument, "namespace is required")
	}
	if req.WorkloadName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workload_name is required")
	}
	workloadKind := req.WorkloadKind
	if workloadKind == "" {
		workloadKind = "Deployment"
	}

	adapter, err := p.server.helm.GetWorkloadAdapter(ctx, req.Namespace, workloadKind, req.WorkloadName)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "workload %s/%s/%s not found: %v", workloadKind, req.Namespace, req.WorkloadName, err)
	}
	workspaceCanonicalId := helm.InjectedCanonicalId(adapter.GetAnnotations())
	if workspaceCanonicalId == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s %s/%s does not have an injected workspace", workloadKind, req.Namespace, req.WorkloadName)
	}

	workspace, err := ws.NewWorkspaceForEject(workspaceCanonicalId, p.server.helm)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to prepare workspace %s: %v", workspaceCanonicalId, err)
	}

	timeout := int(req.TimeoutSeconds)
	if timeout <= 0 {
		timeout = 60
	}

	username := adapter.GetPodTemplate().Labels[helm.LabelUsername]
	if delErr := ws.DeleteWorkspacePAT(ctx, p.server.Identity, username, workspaceCanonicalId); delErr != nil {
		p.log.Error().Err(delErr).Msgf("Failed to delete PAT for workspace %s", workspaceCanonicalId)
	}

	if err := workspace.Eject(ctx, &ws.EjectOptions{
		Namespace:    req.Namespace,
		WorkloadName: req.WorkloadName,
		WorkloadKind: workloadKind,
		Timeout:      timeout,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to eject workspace %s: %v", workspaceCanonicalId, err)
	}

	return &provisionerv1.EjectWorkspaceResponse{
		Workspace: workspaceCanonicalId,
	}, nil
}
