// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"

	"github.com/k8shell-io/common/pkg/models"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// convertToGRPCError converts internal errors to gRPC status errors
func convertToGRPCError(err error) error {
	if errors.Is(err, models.ErrWorkspaceNotFound) {
		return status.Errorf(codes.NotFound, "%s", err.Error())
	}
	if errors.Is(err, models.ErrInvalidParameters) {
		return status.Errorf(codes.InvalidArgument, "%s", err.Error())
	}
	if errors.Is(err, ws.ErrWebProxyAliasHeld) {
		return status.Errorf(codes.AlreadyExists, "%s", err.Error())
	}
	if errors.Is(err, ws.ErrNoWebProxyRoute) {
		return status.Errorf(codes.FailedPrecondition, "%s", err.Error())
	}

	return status.Errorf(codes.Internal, "%s", err.Error())
}

// verifyWorkloadExists checks that a supported workload (Deployment, StatefulSet, DaemonSet)
// with the given kind, namespace, and name exists.
func (p *ProvisionerService) verifyWorkloadExists(ctx context.Context, namespace, kind, name string) error {
	_, err := p.server.helm.GetWorkloadAdapter(ctx, namespace, kind, name)
	if err != nil {
		return status.Errorf(codes.NotFound, "%s %s/%s not found: %v", kind, namespace, name, err)
	}
	return nil
}
