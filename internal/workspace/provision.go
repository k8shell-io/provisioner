package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func (w *Workspace) CanProvision(ctx context.Context) (bool, error) {
	exists, err := w.IsInstalled(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to check if workspace exists: %w", err)
	}

	if exists {
		status, err := w.GetPodStatus(ctx)
		if err != nil {
			if errors.Is(err, models.ErrWorkspaceNotFound) {
				w.log.Warn().Msgf("Workspace is installed but workspace pod %s not found.", w.Name)
			} else {
				return false, fmt.Errorf("failed to get workspace pod status: %w", err)
			}
		} else {
			if status.Status == "Running" {
				return false, nil
			}
		}
	}

	return true, nil
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
		} else {
			if status.Status == "Running" {
				return status, nil
			}
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
func (w *Workspace) watchEvents(ctx context.Context, podName string,
	criticalErrorChan chan<- error, opts *ProvisionOptions) {

	v1 := w.client.KubeClient().CoreV1()

	eventList, err := v1.Events(w.client.TargetNamespace()).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
		Limit:         1,
	})
	if err != nil {
		w.log.Warn().Err(err).Msg("Failed to get current resource version for events, watching from beginning")
	}

	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
		Watch:         true,
	}

	if eventList != nil {
		listOptions.ResourceVersion = eventList.ResourceVersion
	}

	watcher, err := v1.Events(w.client.TargetNamespace()).Watch(ctx, listOptions)
	if err != nil {
		w.log.Warn().Err(err).Msg("Failed to watch events")
		return
	}
	defer watcher.Stop()

	seenEvents := map[string]bool{}

	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}

			if event.Type != watch.Added && event.Type != watch.Modified {
				continue
			}

			k8sEvent, ok := event.Object.(*corev1.Event)
			if !ok || k8sEvent == nil {
				continue
			}

			key := k8sEvent.InvolvedObject.Name + "\x00" + k8sEvent.Message

			if _, exists := seenEvents[key]; exists {
				continue
			}
			seenEvents[key] = true

			eventMessage := models.WorkspaceStreamEvent{
				Type:       "event",
				Timestamp:  k8sEvent.CreationTimestamp.Format("2006-01-02 15:04:05"),
				ObjectName: k8sEvent.InvolvedObject.Name,
				Message:    k8sEvent.Message,
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
		"Failed to pull image":    "Unable to download the workspace image.",
		"ImagePullBackOff":        "Unable to download the workspace image.",
		"ErrImagePull":            "Unable to download the workspace image.",
		"InvalidImageName":        "The workspace image name is invalid.",
		"image not found":         "The specified workspace image was not found in the registry.",
		"authentication required": "Authentication failed when accessing the workspace image.",
		"insufficient memory":     "Not enough memory available to run the workspace.",
		"insufficient cpu":        "Not enough CPU resources available to run the workspace.",
		"no nodes available":      "No suitable servers are available to run the workspace.",
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
