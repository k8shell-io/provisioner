package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/k8shell-io/provisioner/internal/helm"
	provModels "github.com/k8shell-io/provisioner/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// ProvisionOptions represents the options for provisioning a workspace
type ProvisionOptions struct {
	Timeout     int
	Messages    chan provModels.StreamEvent
	LockTimeout int
}

// Provision provisions the workspace
func (w *Workspace) Provision(ctx context.Context, opts *ProvisionOptions) (*provModels.PodStatus, error) {
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

	exists, err := w.IsInstalled(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check if workspace exists: %w", err)
	}

	if exists {
		status, err := w.GetPodStatus(ctx)
		if err != nil {
			if errors.Is(err, provModels.ErrWorkspaceNotFound) {
				w.log.Info().Msgf("Pod %s not found, proceeding with provisioning", w.Name())
			} else {
				return nil, fmt.Errorf("failed to get workspace pod status: %w", err)
			}
		} else {
			if status.Status == "Running" {
				w.log.Info().Msgf("Workspace %s is already running", w.Name())
				return status, nil
			}
		}

		w.log.Info().Msgf("Workspace %s exists but it is not running, need to provision", w.Name())
	} else {
		w.log.Info().Msgf("Workspace %s does not exist, need to provision", w.Name())
	}

	return w.provisionWithLock(ctx, opts)
}

// Lock acquires a distributed lock for the workspace
func (w *Workspace) Lock(timeout time.Duration) error {
	if w.workspaceLock != nil {
		return nil
	}
	w.workspaceLock = NewWorkspaceLock(w.client.GetKubeClient(), w.client.TargetNamespace(), w.Name())

	w.log.Debug().Msgf("Acquiring lock for workspace %s", w.Name())
	lockCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	acquired, err := w.workspaceLock.Acquire(lockCtx)
	if err != nil {
		return fmt.Errorf("failed to acquire lock for workspace %s: %w", w.Name(), err)
	}

	if !acquired {
		return fmt.Errorf("timeout acquiring lock for workspace %s after %f seconds", w.Name(), timeout.Seconds())
	}

	return nil
}

// Unlock releases the distributed lock for the workspace
func (w *Workspace) Unlock() error {
	if w.workspaceLock == nil {
		return nil
	}
	if releaseErr := w.workspaceLock.Release(context.Background()); releaseErr != nil {
		w.log.Error().Err(releaseErr).Msgf("Failed to release lock for workspace %s", w.Name())
	} else {
		w.log.Debug().Msgf("Released lock for workspace %s", w.Name())
	}
	w.workspaceLock = nil
	return nil
}

// provisionWithLock provisions the workspace with a distributed lock
func (w *Workspace) provisionWithLock(ctx context.Context, opts *ProvisionOptions) (*provModels.PodStatus, error) {
	if err := w.Lock(time.Duration(opts.LockTimeout) * time.Second); err != nil {
		return nil, err
	}
	defer func() {
		if releaseErr := w.Unlock(); releaseErr != nil {
			w.log.Error().Err(releaseErr).Msgf("Failed to release lock for workspace %s", w.Name())
		}
	}()

	w.log.Debug().Msgf("Acquired lock for workspace %s", w.Name())

	exists, err := w.IsInstalled(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to recheck if workspace exists: %w", err)
	}

	if exists {
		status, err := w.GetPodStatus(ctx)
		if err != nil {
			if errors.Is(err, provModels.ErrWorkspaceNotFound) {
				w.log.Debug().Msgf("Pod %s not found, proceeding with provisioning", w.Name())
			} else {
				return nil, fmt.Errorf("failed to recheck workspace status: %w", err)
			}
		} else {
			if status.Status == "Running" {
				w.log.Debug().Msgf("Workspace %s is now running (completed by another instance while waiting for lock)", w.Name())
				return status, nil
			}
		}

		w.log.Debug().Msgf("Workspace %s still not running after acquiring lock, proceeding with reinstall", w.Name())
		if err := w.client.Uninstall(w.Name(), int(opts.Timeout)); err != nil {
			return nil, fmt.Errorf("failed to delete workspace: %w", err)
		}
	}

	return w.doInstallation(ctx, opts)
}

func (w *Workspace) doInstallation(ctx context.Context, opts *ProvisionOptions) (*provModels.PodStatus, error) {
	values, err := w.Values()
	if err != nil {
		return nil, err
	}

	if err := w.createHeadlessService(ctx, values); err != nil {
		return nil, fmt.Errorf("failed to create headless service: %w", err)
	}

	startTime := time.Now()
	err = w.client.Install(ctx, helm.WORKSPACE_CHART_NAME, helm.InstallOptions{
		ReleaseName:     w.Name(),
		Values:          values,
		CreateNamespace: false,
		Wait:            false,
		Timeout:         opts.Timeout,
		Labels:          w.Labels(),
		AppVersion:      w.getK8shelldVersion(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to install workspace: %w", err)
	}

	status, err := w.waitForPodRunning(ctx, startTime, opts)
	if err != nil {
		return nil, fmt.Errorf("failed waiting for workspace to be running: %w", err)
	}

	if status.Status == "Running" {
		provisionTime := time.Since(startTime)
		w.log.Info().Msgf("Workspace %s is now running, provisioned in %s", w.Name(), provisionTime)
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

	_, err := w.client.GetKubeClient().CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err == nil {
		w.log.Debug().Msgf("Headless service %s already exists", serviceName)
		return nil
	}

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

	_, err = w.client.GetKubeClient().CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create headless service %s: %w", serviceName, err)
	}

	w.log.Info().Msgf("Created headless service %s for subdomain", serviceName)
	return nil
}

// waitForPodRunning with quick failure detection
func (w *Workspace) waitForPodRunning(ctx context.Context, startTime time.Time,
	opts *ProvisionOptions) (*provModels.PodStatus, error) {

	podName := w.Name()
	timeout := time.NewTimer(time.Duration(opts.Timeout) * time.Second)
	defer timeout.Stop()

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch() // This handles ALL cleanup

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
			case "Running":
				return status, nil

			case "Failed", "Succeeded":
				return status, fmt.Errorf("workspace pod %s is in final state: %s - %s",
					podName, status.Status, status.Message)

			case "Pending":
				if time.Since(startTime) > time.Duration(opts.Timeout)*time.Second {
					return status, fmt.Errorf("workspace pod %s has been pending for too long: %s",
						podName, status.Message)
				}
			}
		}
	}
}

// watchEvents watches and reports Kubernetes events for the pod
func (w *Workspace) watchEvents(ctx context.Context, podName string, criticalErrorChan chan<- error, opts *ProvisionOptions) {
	eventList, err := w.client.GetKubeClient().CoreV1().Events(w.client.TargetNamespace()).List(ctx, metav1.ListOptions{
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

	watcher, err := w.client.GetKubeClient().CoreV1().Events(w.client.TargetNamespace()).Watch(ctx, listOptions)
	if err != nil {
		w.log.Warn().Err(err).Msg("Failed to watch events")
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case event := <-watcher.ResultChan():
			if event.Type == watch.Added || event.Type == watch.Modified {
				if k8sEvent, ok := event.Object.(*corev1.Event); ok {
					eventMessage := provModels.StreamEvent{
						Type:       "event",
						Timestamp:  k8sEvent.CreationTimestamp.Format("2006-01-02 15:04:05"),
						ObjectName: k8sEvent.InvolvedObject.Name,
						Message:    k8sEvent.Message,
					}

					w.log.Info().Msg(eventMessage.String())
					if opts.Messages != nil {
						opts.Messages <- eventMessage
					}

					if w.isCriticalError(eventMessage.Message) {
						criticalErr := fmt.Errorf("provisioning error")
						criticalErrorChan <- criticalErr
						return
					}
				}
			}
		}
	}
}

// isCriticalError determines if an event message indicates a critical error
func (w *Workspace) isCriticalError(message string) bool {
	criticalErrors := []string{
		"Failed to pull image",
		"ImagePullBackOff",
		"ErrImagePull",
		"InvalidImageName",
		"image not found",
		"authentication required",
		"insufficient memory",
		"insufficient cpu",
		"no nodes available",
	}

	messageLower := strings.ToLower(message)
	for _, criticalError := range criticalErrors {
		if strings.Contains(messageLower, strings.ToLower(criticalError)) {
			return true
		}
	}
	return false
}
