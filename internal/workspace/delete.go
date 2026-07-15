// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/identity"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
)

// DeleteWorkspacePod hard-deletes the workspace represented by pod, branching
// on whether it is a standalone Helm-managed workspace or one injected into
// another workload. It acquires the workspace's own distributed lock before
// deleting and releases it when done, mirroring the single-workspace delete
// path so concurrent requests for the same workspace stay serialised.
//
// If that lock is already held — e.g. a concurrent DeleteWorkspace call for
// the same workspace is in flight — it returns ErrLockAlreadyHeld so bulk
// callers can skip the workspace and continue rather than fail the batch.
func DeleteWorkspacePod(ctx context.Context, helmClient *helm.Client, identityClient *identity.IdentityClient,
	cfg *config.Config, pod *corev1.Pod) error {

	if pod.Labels[helm.LabelInjected] == "true" {
		return ejectWorkspacePod(ctx, helmClient, pod)
	}

	name := pod.Name
	w, err := NewWorkspaceFromHelmRelease(ctx, name, helmClient, identityClient, cfg)
	if err != nil {
		return err
	}

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	workspaceLock := w.CreateLock()
	acquired, err := workspaceLock.TryAcquire(lockCtx)
	if err != nil {
		if errors.Is(err, ErrLockAlreadyHeld) {
			return ErrLockAlreadyHeld
		}
		return fmt.Errorf("failed to acquire lock for workspace %s deletion: %w", name, err)
	}
	if !acquired {
		return ErrLockAlreadyHeld
	}
	defer func() {
		if unlockErr := workspaceLock.Release(context.WithoutCancel(ctx)); unlockErr != nil {
			w.log.Error().Err(unlockErr).Msgf("Failed to release lock after deleting workspace %s", name)
		}
	}()

	if err := w.Uninstall(ctx, 10*time.Second, false, false); err != nil {
		return fmt.Errorf("failed to delete workspace %s: %w", name, err)
	}
	return nil
}

func ejectWorkspacePod(ctx context.Context, helmClient *helm.Client, pod *corev1.Pod) error {
	name := pod.Name

	owner, err := FindOwnerWorkload(ctx, helmClient.KubeClient(), pod)
	if err != nil {
		return fmt.Errorf("failed to find owning workload for injected workspace %s: %w", name, err)
	}

	canonicalId := pod.Labels[helm.LabelCanonicalId]
	if canonicalId == "" {
		return fmt.Errorf("injected workspace pod for %s is missing canonical-id label", name)
	}

	w, err := NewWorkspaceForEject(canonicalId, helmClient)
	if err != nil {
		return fmt.Errorf("failed to prepare workspace %s for eject: %w", canonicalId, err)
	}

	if err := w.Eject(ctx, &EjectOptions{
		Namespace:    pod.Namespace,
		WorkloadName: owner.Name,
		WorkloadKind: owner.Kind,
		Timeout:      60,
	}); err != nil {
		return fmt.Errorf("failed to eject workspace %s: %w", name, err)
	}
	return nil
}
