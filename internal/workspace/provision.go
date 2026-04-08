package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// ProvisionOptions represents the options for provisioning a workspace
type ProvisionOptions struct {
	Timeout     int
	Messages    chan models.WorkspaceStreamEvent
	LockTimeout int
}

// ExistsRunning checks if the workspace already exists and is running
func (w *Workspace) ExistsAndRunning(ctx context.Context) (bool, *models.PodStatus, error) {
	exists, err := w.IsInstalled(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("failed to check if workspace exists: %w", err)
	}

	var status *models.PodStatus
	if exists {
		status, err = w.GetPodStatus(ctx)
		if err != nil {
			if errors.Is(err, models.ErrWorkspaceNotFound) {
				w.log.Warn().Msgf("Workspace is installed but workspace pod %s not found.", w.Name)
			} else {
				return false, nil, fmt.Errorf("failed to get workspace pod status: %w", err)
			}
		} else {
			if status.Status == "Running" {
				return true, status, nil
			}
		}
	}

	return false, status, nil
}

// Provision provisions the workspace
func (w *Workspace) Provision(ctx context.Context, opts *ProvisionOptions) (*models.PodStatus, error) {
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

		if status.Status == "Running" {
			return status, nil
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
func (w *Workspace) doInstallation(ctx context.Context, opts *ProvisionOptions) (*models.PodStatus, error) {
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
		"app.kubernetes.io/version":    w.appVersion(),
		"app.kubernetes.io/managed-by": "k8shell-provisioner",
		"k8shell.io/app":               helm.WORKSPACE_CHART_NAME,
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

	status, err := w.waitForPodRunning(ctx, startTime, opts)
	if err != nil {
		return nil, err
	}

	if status.Status == models.WorkspaceStatusRunning {
		provisionTime := time.Since(startTime)
		w.log.Info().Msgf("Workspace %s is now running, provisioned in %s", w.Name, provisionTime)
	}
	return status, nil
}

// doStart re-creates the workspace pod by extracting the pod manifest from the
// stored Helm release and creating the pod directly via the Kubernetes API.
func (w *Workspace) doStart(ctx context.Context, opts *ProvisionOptions) (*models.PodStatus, error) {
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
	status, err := w.waitForPodRunning(ctx, startTime, opts)
	if err != nil {
		return nil, err
	}

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

		if err == nil {
			// PVC already exists — check for parameter mismatches and warn
			existingStorage := existing.Spec.Resources.Requests[corev1.ResourceStorage]
			if existingStorage.String() != storage.Size {
				w.log.Warn().
					Str("pvc", pvcName).
					Str("existingSize", existingStorage.String()).
					Str("requestedSize", storage.Size).
					Msgf("Shared PVC %s already exists with different size; skipping update", pvcName)
			}
			if storage.StorageClass != "" && existing.Spec.StorageClassName != nil &&
				*existing.Spec.StorageClassName != storage.StorageClass {
				w.log.Warn().
					Str("pvc", pvcName).
					Str("existingStorageClass", *existing.Spec.StorageClassName).
					Str("requestedStorageClass", storage.StorageClass).
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
				Annotations: storage.Annotations,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: mustParseQuantity(storage.Size),
					},
				},
			},
		}

		if storage.StorageClass != "" {
			sc := storage.StorageClass
			pvc.Spec.StorageClassName = &sc
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

// waitForPodRunning with quick failure detection
func (w *Workspace) waitForPodRunning(ctx context.Context, startTime time.Time,
	opts *ProvisionOptions) (*models.PodStatus, error) {

	podName := w.Name
	timeout := time.NewTimer(time.Duration(opts.Timeout) * time.Second)
	defer timeout.Stop()

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()

	criticalErrorChan := make(chan error, 1)
	go w.watchEvents(watchCtx, podName, criticalErrorChan, opts)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-timeout.C:
			return nil, fmt.Errorf("timeout waiting for pod %s to be running after %v",
				podName, opts.Timeout)

		case criticalErr := <-criticalErrorChan:
			if criticalErr != nil {
				return nil, criticalErr
			}

		case <-ticker.C:
			status, err := w.GetPodStatus(ctx)
			if err != nil {
				continue
			}

			switch status.Status {
			case models.WorkspaceStatusRunning:
				return status, nil

			case models.WorkspaceStatusFailing, models.WorkspaceStatusStopped:
				return status, fmt.Errorf("workspace %s is in final state: %s - %s",
					podName, status.Status, status.Message)

			case models.WorkspaceStatusProvisioning:
				if time.Since(startTime) > time.Duration(opts.Timeout)*time.Second {
					return status, fmt.Errorf("workspace %s has been starting for too long: %s",
						podName, status.Message)
				}
			}
		}
	}
}

// watchEvents watches and reports Kubernetes events for the pod
// It captures events related to the pod and its PVCs to provide real-time feedback
// and detect critical errors during provisioning
func (w *Workspace) watchEvents(ctx context.Context, podName string,
	criticalErrorChan chan<- error, opts *ProvisionOptions) {

	v1 := w.client.KubeClient().CoreV1()
	namespace := w.client.TargetNamespace()

	eventCh := make(chan *corev1.Event, 256)
	stopOnce := sync.Once{}
	stopAll := func() {
		stopOnce.Do(func() { close(eventCh) })
	}

	startWatcher := func(fieldSelector string) (watch.Interface, error) {
		eventList, err := v1.Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fieldSelector,
			Limit:         1,
		})
		if err != nil {
			w.log.Warn().Err(err).Str("selector",
				fieldSelector).Msg("Failed to list events for resourceVersion; watching from beginning")
		}

		listOptions := metav1.ListOptions{
			FieldSelector: fieldSelector,
			Watch:         true,
		}
		if eventList != nil {
			listOptions.ResourceVersion = eventList.ResourceVersion
		}

		watcher, err := v1.Events(namespace).Watch(ctx, listOptions)
		if err != nil {
			return nil, err
		}
		return watcher, nil
	}

	podSelector := fmt.Sprintf("involvedObject.kind=Pod,involvedObject.name=%s", podName)
	podWatcher, err := startWatcher(podSelector)
	if err != nil {
		w.log.Warn().Err(err).Msg("Failed to watch pod events")
	} else {
		defer podWatcher.Stop()
		go func() {
			defer func() {
				// If both watchers exit, main loop will end via ctx.Done.
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case evt, ok := <-podWatcher.ResultChan():
					if !ok {
						return
					}
					if evt.Type != watch.Added && evt.Type != watch.Modified {
						continue
					}
					k8sEvent, ok := evt.Object.(*corev1.Event)
					if !ok || k8sEvent == nil {
						continue
					}

					select {
					case eventCh <- k8sEvent:
					case <-ctx.Done():
						return
					default:
						w.log.Debug().Msg("Dropping event due to full channel")
					}
				}
			}
		}()
	}

	pvcSelector := "involvedObject.kind=PersistentVolumeClaim"
	pvcWatcher, err := startWatcher(pvcSelector)
	if err != nil {
		w.log.Warn().Err(err).Msg("Failed to watch PVC events")
	} else {
		defer pvcWatcher.Stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case evt, ok := <-pvcWatcher.ResultChan():
					if !ok {
						return
					}
					if evt.Type != watch.Added && evt.Type != watch.Modified {
						continue
					}
					k8sEvent, ok := evt.Object.(*corev1.Event)
					if !ok || k8sEvent == nil {
						continue
					}
					select {
					case eventCh <- k8sEvent:
					case <-ctx.Done():
						return
					default:
						w.log.Debug().Msg("Dropping event due to full channel")
					}
				}
			}
		}()
	}

	defer stopAll()

	seenEvents := map[string]bool{}
	pvcPrefix := "pvc-" + podName + "-"

	for {
		select {
		case <-ctx.Done():
			return

		case k8sEvent, ok := <-eventCh:
			if !ok {
				return
			}
			if k8sEvent == nil {
				continue
			}

			kind := k8sEvent.InvolvedObject.Kind
			name := k8sEvent.InvolvedObject.Name

			switch kind {
			case "Pod":
				if name != podName {
					continue
				}
			case "PersistentVolumeClaim":
				if !strings.HasPrefix(name, pvcPrefix) {
					continue
				}
			default:
				continue
			}

			message := k8sEvent.Message
			if k8sEvent.Reason != "" {
				message = fmt.Sprintf("%s: %s", k8sEvent.Reason, k8sEvent.Message)
			}

			key := kind + "\x00" + name + "\x00" + message
			if _, exists := seenEvents[key]; exists {
				continue
			}
			seenEvents[key] = true

			eventMessage := models.WorkspaceStreamEvent{
				Type:       "event",
				Timestamp:  k8sEvent.CreationTimestamp.Format("2006-01-02 15:04:05"),
				ObjectName: fmt.Sprintf("%s/%s", kind, name),
				Message:    message,
			}

			w.log.Debug().Msg(eventMessage.String())
			if opts.Messages != nil {
				opts.Messages <- eventMessage
			}

			if criticalErr := w.isCriticalError(eventMessage.Message); criticalErr != nil {
				criticalErrorChan <- criticalErr
				return
			}
		}
	}
}

// isCriticalError determines if an event message indicates a critical error and returns a user-friendly error
func (w *Workspace) isCriticalError(message string) error {
	criticalErrors := map[string]string{
		// workspace image pull errors
		"Failed to pull image":    "Unable to download the workspace image (code 1).",
		"ImagePullBackOff":        "Unable to download the workspace image (code 2).",
		"ErrImagePull":            "Unable to download the workspace image (code 3).",
		"InvalidImageName":        "The workspace image name is invalid (code 4).",
		"image not found":         "The workspace image was not found in the registry (code 5).",
		"authentication required": "Authentication failed when accessing the workspace image (code 6).",

		// resource issues
		"insufficient memory": "Not enough memory available to run the workspace (code 7).",
		"insufficient cpu":    "Not enough CPU resources available to run the workspace (code 8).",
		"no nodes available":  "No suitable servers are available to run the workspace (code 9).",

		// storage issues
		"unbound immediate persistentvolumeclaims": "Workspace is waiting for storage (code 10).",
		// (PVCs are unbound). Check StorageClass/provisioner and PV availability.

		"failedbinding": "Workspace storage could not be provisioned (code 11).",
		//  Check PV capacity/access modes and StorageClass.

		"failed to provision volume": "Workspace storage could not be provisioned (code 12).",
		// Check CSI controller logs and StorageClass.

		"provisioningfailed": "Workspace storage provisioning error (code 13).",
		// Check CSI controller logs and StorageClass.

		"failed scheduling": "Workspace could not be scheduled (code 14).",
		// often due to unbound PVCs or insufficient resources
	}

	messageLower := strings.ToLower(message)
	for criticalError, userMessage := range criticalErrors {
		if strings.Contains(messageLower, strings.ToLower(criticalError)) {
			w.log.Error().Msgf("Provisioning error detected: %s", message)
			provError := fmt.Sprintf("Provisioning error: %s", userMessage)
			return fmt.Errorf("%s", provError)
		}
	}
	return nil
}
