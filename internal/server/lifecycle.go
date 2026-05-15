package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/provisioner/internal/helm"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
		workspace, err := p.prepareWorkspaceWithPod(ctx, pod)
		if err != nil {
			return nil, err
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
	workspaceCanonicalId := adapter.GetAnnotations()[helm.AnnotationInjectedCanonicalId]
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
