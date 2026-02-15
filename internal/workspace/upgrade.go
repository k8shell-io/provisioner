package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var ErrInvalidValue = errors.New("invalid value")

// Upgrade upgrades an existing workspace release (or installs it if missing).
// It uses a distributed lock to avoid concurrent upgrades for the same workspace.
func (w *Workspace) Upgrade(ctx context.Context, opts *ProvisionOptions) (*models.PodStatus, error) {
	if opts == nil {
		opts = &ProvisionOptions{
			Timeout:     20,
			Messages:    nil,
			LockTimeout: 30,
		}
	}
	if opts.LockTimeout == 0 {
		opts.LockTimeout = 30
	}

	return w.upgradeWithLock(ctx, opts)
}

func (w *Workspace) upgradeWithLock(ctx context.Context, opts *ProvisionOptions) (*models.PodStatus, error) {
	if err := w.lock(time.Duration(opts.LockTimeout) * time.Second); err != nil {
		return nil, err
	}
	defer func() {
		if releaseErr := w.unlock(); releaseErr != nil {
			w.log.Error().Err(releaseErr).Msgf("Failed to release lock for workspace %s", w.Name)
		}
	}()

	values, err := w.Values()
	if err != nil {
		return nil, err
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       helm.WORKSPACE_CHART_NAME,
		"app.kubernetes.io/instance":   w.Name,
		"app.kubernetes.io/version":    w.appVersion(),
		"app.kubernetes.io/managed-by": "k8shell-provisioner",
		"k8shell.io/app":               helm.WORKSPACE_CHART_NAME,
		"k8shell.io/workspace":         w.Name,
		"k8shell.io/username":          w.user.Username,
		"k8shell.io/blueprint":         w.blueprint.Name,
		"k8shell.io/organization":      w.user.Organization,
	}

	startTime := time.Now()
	if err := w.client.Upgrade(ctx, helm.InstallOptions{
		ReleaseName: w.Name,
		ChartName:   helm.WORKSPACE_CHART_NAME,
		Values:      values,
		Wait:        false,
		Timeout:     opts.Timeout,
		Labels:      labels,
		AppVersion:  w.appVersion(),
	}); err != nil {
		return nil, fmt.Errorf("failed to upgrade workspace %s: %w", w.Name, err)
	}

	status, err := w.waitForPodRunning(ctx, startTime, opts)
	if err != nil {
		return nil, err
	}

	if status.Status == "Running" {
		upgradeTime := time.Since(startTime)
		w.log.Info().Msgf("Workspace %s upgrade completed in %s", w.Name, upgradeTime)
	}

	return status, nil
}

// ResizeResources resizes k8shell-main container CPU/memory using the Pod /resize subresource.
// If cpu or memory is empty (or "0"), that resource is left unchanged.
func (w *Workspace) ResizeResources(ctx context.Context, cpu, memory string) error {
	cpu = strings.TrimSpace(cpu)
	memory = strings.TrimSpace(memory)

	// validate inputs (only if provided)
	if cpu != "" && cpu != "0" {
		if err := validateCPUQuantity(cpu); err != nil {
			return fmt.Errorf("%w: invalid cpu %q: %v", ErrInvalidValue, cpu, err)
		}
	}
	if memory != "" && memory != "0" {
		if err := validateMemoryQuantity(memory); err != nil {
			return fmt.Errorf("%w: invalid memory %q: %v", ErrInvalidValue, memory, err)
		}
	}

	if (cpu == "" || cpu == "0") && (memory == "" || memory == "0") {
		return nil
	}

	ns := w.client.TargetNamespace()
	v1 := w.client.KubeClient().CoreV1()
	lim := map[string]string{}

	// Only set the keys that were specified; unspecified ones remain unchanged server-side.
	if cpu != "" && cpu != "0" {
		lim["cpu"] = cpu
	}
	if memory != "" && memory != "0" {
		lim["memory"] = memory
	}

	patchObj := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name": "k8shell-main",
					"resources": map[string]any{
						"limits": lim,
					},
				},
			},
		},
	}

	patchBytes, err := json.Marshal(patchObj)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	_, err = v1.Pods(ns).Patch(
		ctx,
		w.Name,
		types.StrategicMergePatchType,
		patchBytes,
		metav1.PatchOptions{},
		"resize",
	)
	if err != nil {
		if strings.Contains(err.Error(), "Invalid value") {
			return fmt.Errorf("%w: %v", ErrInvalidValue, err)
		}
		return fmt.Errorf("patch workspace resources via /resize: %w", err)
	}

	return nil
}

func validateCPUQuantity(v string) error {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return err
	}
	if q.Sign() <= 0 {
		return fmt.Errorf("must be > 0")
	}
	return nil
}

func validateMemoryQuantity(v string) error {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return err
	}
	if q.Sign() <= 0 {
		return fmt.Errorf("must be > 0")
	}
	return nil
}
