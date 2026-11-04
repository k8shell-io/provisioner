// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/provisioner/internal/helm"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SetWorkspaceSchedule overrides the schedule of a running workspace, or
// restores its blueprint's. It does not authorize the caller: like the other
// internal RPCs, that is up to the calling service.
func (p *ProvisionerService) SetWorkspaceSchedule(ctx context.Context,
	req *provisionerv1.SetWorkspaceScheduleRequest) (*provisionerv1.SetWorkspaceScheduleResponse, error) {

	name := req.GetWorkspace()
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace name is required")
	}
	change := ws.ScheduleChange{
		To:             req.To,
		StopGrace:      req.StopGrace,
		TerminateGrace: req.TerminateGrace,
		Revert:         req.GetRevert(),
	}
	hasValues := change.To != nil || change.StopGrace != nil || change.TerminateGrace != nil
	switch {
	case change.Revert && hasValues:
		return nil, status.Errorf(codes.InvalidArgument, "revert cannot be combined with schedule values")
	case !change.Revert && !hasValues:
		return nil, status.Errorf(codes.InvalidArgument, "at least one of to, stop_grace, terminate_grace or revert must be set")
	}

	if _, pod, findErr := ws.FindWorkspace(ctx, p.server.helm, name, p.server.config.InjectNamespaces, false); findErr == nil &&
		pod.Labels[helm.LabelInjected] == "true" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"workspace %s is injected into a workload and its schedule cannot be changed", name)
	}

	w, err := ws.NewWorkspaceFromHelmRelease(ctx, name, p.server.helm, p.server.Identity, p.server.config)
	if err != nil {
		return nil, convertToGRPCError(err)
	}

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lock := w.CreateLock()
	acquired, err := lock.TryAcquire(lockCtx)
	if err != nil {
		if errors.Is(err, ws.ErrLockAlreadyHeld) {
			return nil, status.Errorf(codes.Aborted, "another operation on workspace %s is in progress", name)
		}
		return nil, status.Errorf(codes.Internal, "failed to acquire lock for workspace %s: %v", name, err)
	}
	if !acquired {
		return nil, status.Errorf(codes.Aborted, "another operation on workspace %s is in progress", name)
	}
	defer func() {
		if err := lock.Release(context.WithoutCancel(ctx)); err != nil {
			p.log.Error().Err(err).Msgf("Failed to release lock after changing the schedule of workspace %s", name)
		}
	}()

	schedule, err := w.SetSchedule(ctx, change, time.Now())
	if err != nil {
		return nil, convertToGRPCError(err)
	}

	msg := fmt.Sprintf("Schedule of workspace %s was changed", name)
	switch {
	case change.Revert:
		msg = fmt.Sprintf("Schedule of workspace %s was reverted to its blueprint's", name)
	case schedule == nil || schedule.To == nil:
		msg = fmt.Sprintf("Schedule of workspace %s was changed; it has no expiry", name)
	}
	p.log.Info().Str("workspace", name).Bool("revert", change.Revert).Msg("Workspace schedule changed")
	return &provisionerv1.SetWorkspaceScheduleResponse{
		Message:  msg,
		Schedule: gapi.WorkspaceScheduleToProto(schedule),
	}, nil
}
