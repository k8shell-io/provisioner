package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProvisionOptions represents the options for provisioning a workspace
type ProvisionOptions struct {
	Timeout     int
	Messages    chan models.WorkspaceStreamEvent
	LockTimeout int
}

// ExistsRunning checks if the workspace already exists and is running
func (w *Workspace) ExistsAndRunning(ctx context.Context) (bool, *models.WorkspaceStatus, error) {
	exists, err := w.IsInstalled(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("failed to check if workspace exists: %w", err)
	}

	var status *models.WorkspaceStatus
	if exists {
		status, err = w.GetPodStatus(ctx)
		if err != nil {
			if errors.Is(err, models.ErrWorkspaceNotFound) {
				w.log.Warn().Msgf("Workspace is installed but workspace pod %s not found.", w.Name)
			} else {
				return false, nil, fmt.Errorf("failed to get workspace pod status: %w", err)
			}
		} else {
			if status.Status == models.WorkspaceStatusRunning {
				return true, status, nil
			}
		}
	}

	return false, status, nil
}

// Provision provisions the workspace
func (w *Workspace) Provision(ctx context.Context, opts *ProvisionOptions) (*models.WorkspaceStatus, error) {
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

	if preExists, _ := w.IsInstalled(ctx); preExists {
		if preStatus, err := w.GetPodStatus(ctx); err == nil {
			switch preStatus.Status {
			case models.WorkspaceStatusRunning:
				return preStatus, nil
			case models.WorkspaceStatusProvisioning, models.WorkspaceStatusPulling:
				w.log.Info().Msgf("Workspace %s is already being provisioned, observing", w.Name)
				pw := NewPodWatcher(w.client.KubeClient(), w.client.TargetNamespace(), w.Name, w.log)
				snap, err := pw.Watch(ctx, opts, true)
				if err != nil {
					return nil, err
				}
				return snapToWorkspaceStatus(snap), nil
			}
		}
	}

	if err := w.lock(time.Duration(opts.LockTimeout) * time.Second); err != nil {
		return nil, err
	}
	defer func() {
		if releaseErr := w.unlock(); releaseErr != nil {
			w.log.Error().Err(releaseErr).Msgf("Failed to release lock for workspace %s", w.Name)
		}
	}()

	w.log.Debug().Msgf("Acquired lock for workspace %s", w.Name)

	exists, err := w.IsInstalled(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to recheck if workspace exists: %w", err)
	}

	if exists {
		status, err := w.GetPodStatus(ctx)
		if err != nil {
			if !errors.Is(err, models.ErrWorkspaceNotFound) {
				return nil, fmt.Errorf("failed to recheck workspace status: %w", err)
			}
			// Helm release exists but pod is gone — re-create only the pod
			w.log.Debug().Msgf("Workspace %s helm release exists but pod not found, starting pod", w.Name)
			return w.doStart(ctx, opts)
		}

		if status.Status == models.WorkspaceStatusRunning {
			return status, nil
		}

		if status.Status == models.WorkspaceStatusPulling {
			w.log.Info().Msgf("Workspace %s pod is pulling image, waiting", w.Name)
			pw := NewPodWatcher(w.client.KubeClient(), w.client.TargetNamespace(), w.Name, w.log)
			snap, err := pw.Watch(ctx, opts, true)
			if err != nil {
				return nil, err
			}
			return snapToWorkspaceStatus(snap), nil
		}

		if status.Status == models.WorkspaceStatusStopped {
			w.log.Info().Msgf("Workspace %s pod is stopped, recycling pod", w.Name)
			if err := w.StopPod(ctx); err != nil {
				return nil, fmt.Errorf("failed to delete stopped pod: %w", err)
			}
			return w.doStart(ctx, opts)
		}

		w.log.Debug().Msgf("Workspace %s still not running after acquiring lock, proceeding with reinstall", w.Name)
		if err := w.client.Uninstall(w.Name, int(opts.Timeout), true); err != nil {
			return nil, fmt.Errorf("failed to delete workspace: %w", err)
		}
	}

	return w.doInstallation(ctx, opts)
}

// Lock acquires a distributed lock for the workspace
func (w *Workspace) lock(timeout time.Duration) error {
	if w.workspaceLock != nil {
		return nil
	}
	w.workspaceLock = w.CreateLock()

	w.log.Debug().Msgf("Acquiring lock for workspace %s", w.Name)
	lockCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	acquired, err := w.workspaceLock.Acquire(lockCtx)
	if err != nil {
		return fmt.Errorf("failed to acquire lock for workspace %s: %w", w.Name, err)
	}

	if !acquired {
		return fmt.Errorf("timeout acquiring lock for workspace %s after %f seconds", w.Name, timeout.Seconds())
	}

	return nil
}

// Unlock releases the distributed lock for the workspace
func (w *Workspace) unlock() error {
	if w.workspaceLock == nil {
		return nil
	}
	if releaseErr := w.workspaceLock.Release(context.Background()); releaseErr != nil {
		w.log.Error().Err(releaseErr).Msgf("Failed to release lock for workspace %s", w.Name)
	} else {
		w.log.Debug().Msgf("Released lock for workspace %s", w.Name)
	}
	w.workspaceLock = nil
	return nil
}

// doInstallation performs the actual installation of the workspace
func (w *Workspace) doInstallation(ctx context.Context, opts *ProvisionOptions) (*models.WorkspaceStatus, error) {
	if err := w.ensureSharedStorages(ctx); err != nil {
		return nil, fmt.Errorf("failed to ensure shared storages: %w", err)
	}

	values, err := w.Values()
	if err != nil {
		return nil, err
	}

	values["__manifesthash__"], err = w.TemplateHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to compute template hash: %w", err)
	}

	if err := w.createHeadlessService(ctx, values); err != nil {
		return nil, fmt.Errorf("failed to create headless service: %w", err)
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       helm.WORKSPACE_CHART_NAME,
		"app.kubernetes.io/instance":   w.Name,
		"k8shell.io/k8shelld-version":  w.appVersion(),
		"app.kubernetes.io/managed-by": "k8shell-provisioner",
		"k8shell.io/type":              "workspace",
		"k8shell.io/workspace":         w.Name,
		"k8shell.io/username":          w.user.Username,
		"k8shell.io/blueprint":         w.blueprint.Name,
		"k8shell.io/organization":      w.user.Organization,
	}

	startTime := time.Now()
	err = w.client.Install(ctx, helm.WORKSPACE_CHART_NAME, helm.InstallOptions{
		ReleaseName:     w.Name,
		Values:          values,
		CreateNamespace: false,
		Wait:            false,
		Timeout:         opts.Timeout,
		Labels:          labels,
		AppVersion:      w.appVersion(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to install workspace: %w", err)
	}

	pw := NewPodWatcher(w.client.KubeClient(), w.client.TargetNamespace(), w.Name, w.log)
	snap, err := pw.Watch(ctx, opts, true)
	if err != nil {
		return nil, err
	}
	status := snapToWorkspaceStatus(snap)
	if status.Status == models.WorkspaceStatusRunning {
		provisionTime := time.Since(startTime)
		w.log.Info().Msgf("Workspace %s is now running, provisioned in %s", w.Name, provisionTime)
	}
	return status, nil
}

// doStart re-creates the workspace pod by extracting the pod manifest from the
// stored Helm release and creating the pod directly via the Kubernetes API.
func (w *Workspace) doStart(ctx context.Context, opts *ProvisionOptions) (*models.WorkspaceStatus, error) {
	pod, err := w.client.PodFromRelease(w.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod manifest from release: %w", err)
	}

	namespace := w.client.TargetNamespace()
	_, err = w.client.KubeClient().CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create workspace pod: %w", err)
	}

	startTime := time.Now()
	pw := NewPodWatcher(w.client.KubeClient(), w.client.TargetNamespace(), w.Name, w.log)
	snap, err := pw.Watch(ctx, opts, true)
	if err != nil {
		return nil, err
	}
	status := snapToWorkspaceStatus(snap)
	if status.Status == models.WorkspaceStatusRunning {
		w.log.Info().Msgf("Workspace %s pod started in %s", w.Name, time.Since(startTime))
	}
	return status, nil
}

// ensureSharedStorages creates PVCs for storages of type "shared" if they don't already exist.
// Shared PVCs are named pvc-<storageName> and are not workspace-scoped, allowing multiple
// workspaces to reference the same PVC. If a shared PVC already exists, it is left untouched.
// A warning is logged when an existing PVC has different capacity or storage class.
func (w *Workspace) ensureSharedStorages(ctx context.Context) error {
	if w.blueprint == nil {
		return nil
	}

	namespace := w.client.TargetNamespace()
	kubeClient := w.client.KubeClient()

	for name, storage := range w.blueprint.Storages {
		if !storage.Enabled || storage.Type != "shared" {
			continue
		}

		pvcName := "pvc-" + name
		if storage.Id != "" {
			pvcName += "-" + storage.Id
		}

		existing, err := kubeClient.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check shared PVC %s: %w", pvcName, err)
		}

		var claimSpec corev1.PersistentVolumeClaimSpec
		if storage.ClaimSpec != nil {
			jsonRaw, err := json.Marshal(storage.ClaimSpec)
			if err != nil {
				return fmt.Errorf("storage %q: failed to marshal claimSpec: %w", name, err)
			}
			if err := json.Unmarshal(jsonRaw, &claimSpec); err != nil {
				return fmt.Errorf("storage %q: invalid claimSpec: %w", name, err)
			}
		}

		if err == nil {
			// PVC already exists — check for parameter mismatches and warn
			existingStorage := existing.Spec.Resources.Requests[corev1.ResourceStorage]
			if requestedStorage, ok := claimSpec.Resources.Requests[corev1.ResourceStorage]; ok {
				if existingStorage.String() != requestedStorage.String() {
					w.log.Warn().
						Str("pvc", pvcName).
						Str("existingSize", existingStorage.String()).
						Str("requestedSize", requestedStorage.String()).
						Msgf("Shared PVC %s already exists with different size; skipping update", pvcName)
				}
			}
			if claimSpec.StorageClassName != nil && existing.Spec.StorageClassName != nil &&
				*existing.Spec.StorageClassName != *claimSpec.StorageClassName {
				w.log.Warn().
					Str("pvc", pvcName).
					Str("existingStorageClass", *existing.Spec.StorageClassName).
					Str("requestedStorageClass", *claimSpec.StorageClassName).
					Msgf("Shared PVC %s already exists with different storageClass; skipping update", pvcName)
			}
			continue
		}

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/version":     w.client.AppVersion,
					"app.kubernetes.io/managed-by":  "k8shell-provisioner",
					"k8shell.io/storage-type":       "shared",
					"k8shell.io/storage-name":       name,
					"io.k8shell.provisioner/commit": w.client.Commit,
				},
				Annotations: storage.ClaimSpecAnnotations,
			},
			Spec: claimSpec,
		}

		if _, err := kubeClient.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
			return fmt.Errorf("storage %q: claimSpec rejected by API server (dry-run): %w", name, err)
		}

		if _, err := kubeClient.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				w.log.Debug().Msgf("Shared PVC %s was created concurrently; skipping", pvcName)
				continue
			}
			return fmt.Errorf("failed to create shared PVC %s: %w", pvcName, err)
		}

		w.log.Info().Msgf("Created shared PVC %s in namespace %s", pvcName, namespace)
	}

	return nil
}

// createHeadlessService creates a headless service if subdomain and hostname are defined
func (w *Workspace) createHeadlessService(ctx context.Context, values map[string]interface{}) error {
	subdomain, hasSubdomain := values["subdomain"].(string)
	hostname, hasHostname := values["hostname"].(string)

	if !hasSubdomain || !hasHostname || subdomain == "" || hostname == "" {
		w.log.Debug().Msg("Subdomain or hostname not defined, skipping headless service creation")
		return nil
	}

	serviceName := subdomain
	namespace := w.client.TargetNamespace()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k8shell-provisioner",
				"k8shell.io/component":         "headless-service",
				"k8shell.io/subdomain":         subdomain,
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector: map[string]string{
				"k8shell.io/subdomain": subdomain,
			},
		},
	}

	_, err := w.client.KubeClient().CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			w.log.Info().Msgf("Headless service %s already exists", serviceName)
			return nil
		}
		return fmt.Errorf("failed to create headless service %s: %w", serviceName, err)
	}

	w.log.Info().Msgf("Created headless service %s for subdomain", serviceName)
	return nil
}
