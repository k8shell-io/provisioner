package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// verifyDeploymentExists checks that a Deployment with the given name exists in namespace.
func (p *ProvisionerService) verifyDeploymentExists(ctx context.Context, namespace, deploymentName string) error {
	_, err := p.server.helm.KubeClient().AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			return status.Errorf(codes.NotFound, "deployment %s/%s not found", namespace, deploymentName)
		}
		return status.Errorf(codes.Internal, "failed to get deployment %s/%s: %v", namespace, deploymentName, err)
	}
	return nil
}
