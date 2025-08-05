package workspace

import (
	"context"
	"fmt"
	"time"

	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type EventMessage struct {
	Timestamp  string
	ObjectName string
	Message    string
}

func (e EventMessage) String() string {
	return fmt.Sprintf("[%s] [%-12s] %s",
		e.Timestamp, e.ObjectName, e.Message)
}

type ProvisionOptions struct {
	Timeout     int
	Messages    chan EventMessage
	LockTimeout int
}

func (w *Workspace) Provision(ctx context.Context, opts *ProvisionOptions) (*WorkspaceStatus, error) {
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
		status, err := w.GetStatus(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get workspace status: %w", err)
		}

		if status.Status == "Running" {
			w.log.Info().Msgf("Workspace %s is already running, no provisioning needed", w.Name())
			return status, nil
		}

		w.log.Info().Msgf("Workspace %s exists but is not running (%s), need to provision", w.Name(), status.Status)
	} else {
		w.log.Info().Msgf("Workspace %s does not exist, need to provision", w.Name())
	}

	return w.provisionWithLock(ctx, opts)
}

func (w *Workspace) provisionWithLock(ctx context.Context, opts *ProvisionOptions) (*WorkspaceStatus, error) {
	workspaceLock := NewWorkspaceLock(w.client.GetKubeClient(), w.client.TargetNamespace(), w.Name())

	w.log.Info().Msgf("Acquiring lock for workspace %s provisioning", w.Name())
	lockCtx, cancel := context.WithTimeout(ctx, time.Duration(opts.LockTimeout)*time.Second)
	defer cancel()

	acquired, err := workspaceLock.Acquire(lockCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock for workspace %s: %w", w.Name(), err)
	}

	if !acquired {
		return nil, fmt.Errorf("timeout acquiring lock for workspace %s after %d seconds", w.Name(), opts.LockTimeout)
	}

	defer func() {
		if releaseErr := workspaceLock.Release(context.Background()); releaseErr != nil {
			w.log.Error().Err(releaseErr).Msgf("Failed to release lock for workspace %s", w.Name())
		} else {
			w.log.Info().Msgf("Released lock for workspace %s", w.Name())
		}
	}()

	w.log.Info().Msgf("Acquired lock for workspace %s", w.Name())

	exists, err := w.IsInstalled(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to recheck if workspace exists: %w", err)
	}

	if exists {
		status, err := w.GetStatus(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to recheck workspace status: %w", err)
		}

		if status.Status == "Running" {
			w.log.Info().Msgf("Workspace %s is now running (completed by another instance while waiting for lock)", w.Name())
			return status, nil
		}

		w.log.Info().Msgf("Workspace %s still not running after acquiring lock, proceeding with reinstall", w.Name())
		if err := w.client.Uninstall(w.Name(), int(opts.Timeout)); err != nil {
			return nil, fmt.Errorf("failed to delete workspace: %w", err)
		}
	}

	return w.doInstallation(ctx, opts)
}

func (w *Workspace) doInstallation(ctx context.Context, opts *ProvisionOptions) (*WorkspaceStatus, error) {
	values, err := w.Values()
	if err != nil {
		return nil, err
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
		status.ProvisionTime = time.Since(startTime)
		w.log.Info().Msgf("Workspace %s is now running, provisioned in %s", w.Name(), status.ProvisionTime)
	}
	return status, nil
}

// waitForPodRunning with quick failure detection
func (w *Workspace) waitForPodRunning(ctx context.Context, startTime time.Time,
	opts *ProvisionOptions) (*WorkspaceStatus, error) {

	podName := w.Name()
	timeout := time.NewTimer(time.Duration(opts.Timeout) * time.Second)
	defer timeout.Stop()

	eventStop := make(chan struct{})
	go w.watchEvents(ctx, podName, eventStop, opts)
	defer close(eventStop)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-timeout.C:
			return nil, fmt.Errorf("timeout waiting for pod %s to be running after %v",
				podName, opts.Timeout)

		case <-ticker.C:
			status, err := w.GetStatus(ctx)
			if err != nil {
				continue // Keep trying
			}

			switch status.Status {
			case "Running":
				return status, nil

			case "Failed", "Succeeded":
				return status, fmt.Errorf("pod %s is in final state: %s - %s",
					podName, status.Status, status.Message)

			case "Pending":
				// Check if we've been pending too long
				if time.Since(startTime) > time.Duration(opts.Timeout)*time.Second {
					return status, fmt.Errorf("pod %s has been pending for too long: %s",
						podName, status.Message)
				}
			}
		}
	}
}

// watchEvents watches and reports Kubernetes events for the pod
func (w *Workspace) watchEvents(ctx context.Context, podName string, stop <-chan struct{}, opts *ProvisionOptions) {
	watcher, err := w.client.GetKubeClient().CoreV1().Events(w.client.TargetNamespace()).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
	})
	if err != nil {
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case event := <-watcher.ResultChan():
			if event.Type == watch.Added || event.Type == watch.Modified {
				if k8sEvent, ok := event.Object.(*corev1.Event); ok {
					eventMessage := EventMessage{
						Timestamp:  k8sEvent.CreationTimestamp.Format("2006-01-02 15:04:05"),
						ObjectName: k8sEvent.InvolvedObject.Name,
						Message:    k8sEvent.Message,
					}
					w.log.Info().Msg(eventMessage.String())
					if opts.Messages != nil {
						opts.Messages <- eventMessage
					}
				}
			}
		}
	}
}
