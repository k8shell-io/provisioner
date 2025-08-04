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
	Timeout  time.Duration
	Messages chan EventMessage
}

func (w *Workspace) Provision(ctx context.Context, opts *ProvisionOptions) (*WorkspaceStatus, error) {
	if opts == nil {
		opts = &ProvisionOptions{
			Timeout:  5 * time.Minute,
			Messages: nil,
		}
	}

	values, err := w.Values()
	if err != nil {
		return nil, err
	}

	// Check if workspace already exists
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
			return status, nil
		}

		w.log.Info().Msgf("Workspace %s already exists but is not running, attempting to reinstall", w.Name())
		if err := w.client.Uninstall(w.Name(), w.Namespace(), true); err != nil {
			return nil, fmt.Errorf("failed to delete workspace: %w", err)
		}
	}

	startTime := time.Now()
	err = w.client.Install(ctx, helm.WORKSPACE_CHART_NAME, helm.InstallOptions{
		ReleaseName: w.Name(),
		Namespace:   w.Namespace(),
		Values:      values,
		Wait:        false,
		Timeout:     int(opts.Timeout.Seconds()),
		Labels:      w.Labels(),
		AppVersion:  w.getK8shelldVersion(),
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
		return status, nil
	}

	return nil, fmt.Errorf("workspace failed to reach running state: %s", status.Status)
}

// waitForPodRunning waits for the workspace pod to be in running state
func (w *Workspace) waitForPodRunning(ctx context.Context, startTime time.Time,
	opts *ProvisionOptions) (*WorkspaceStatus, error) {

	podName := w.Name()
	timeout := time.NewTimer(opts.Timeout)
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
				if time.Since(startTime) > opts.Timeout {
					return status, fmt.Errorf("pod %s has been pending for too long: %s",
						podName, status.Message)
				}
			}
		}
	}
}

// watchEvents watches and reports Kubernetes events for the pod
func (w *Workspace) watchEvents(ctx context.Context, podName string, stop <-chan struct{}, opts *ProvisionOptions) {
	watcher, err := w.client.GetKubeClient().CoreV1().Events(w.Namespace()).Watch(ctx, metav1.ListOptions{
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
