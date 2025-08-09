package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	identity "github.com/k8shell-io/identity/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/k8shell-io/provisioner/pkg/models"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Workspace represents a workspace with Helm client
type Workspace struct {
	log       *zerolog.Logger
	client    *helm.Client
	blueprint *models.Blueprint
	user      *identity.User
}

// ErrWorkspaceNotFound is returned when a workspace is not found
var ErrWorkspaceNotFound = errors.New("workspace not found")

// ErrInvalidParameters is returned when the provided parameters are invalid
var ErrInvalidParameters = errors.New("invalid parameters")

// GetWorkspaceInfo retrieves information about workspaces
func GetWorkspaceInfo(helmClient *helm.Client, name string, username string, blueprint string) ([]models.WorkspaceInfo, error) {
	labels := map[string]string{
		"app.kubernetes.io/name": helm.WORKSPACE_CHART_NAME,
	}

	if name != "" {
		labels["app.kubernetes.io/instance"] = name
	}

	if username != "" {
		labels["k8shell.io/username"] = username
	}

	if blueprint != "" {
		labels["k8shell.io/blueprint"] = blueprint
	}

	selector := GetSelector(labels)
	releases, err := helmClient.ListWithSelector(helmClient.TargetNamespace(), selector)
	if err != nil {
		if strings.Contains(err.Error(), "unable to parse") {
			return nil, fmt.Errorf("failed to list releases: %w", ErrInvalidParameters)
		}
		return nil, fmt.Errorf("failed to list releases: %w", err)
	}

	resp := make([]models.WorkspaceInfo, 0, len(releases))
	for _, release := range releases {
		resp = append(resp, models.WorkspaceInfo{
			Name:      release.Labels["app.kubernetes.io/instance"],
			Username:  release.Labels["k8shell.io/username"],
			Blueprint: release.Labels["k8shell.io/blueprint"],
			Deployed:  release.Info.LastDeployed.Time,
		})
	}
	return resp, nil
}

func GetWorkspaceStatus(ctx context.Context, helmClient *helm.Client, name string) (*models.WorkspaceStatus, error) {
	v1 := helmClient.GetKubeClient().CoreV1()

	var pod *corev1.Pod
	var keysSecret *corev1.Secret
	var tlsSecret *corev1.Secret

	g, ctx := errgroup.WithContext(ctx)

	// Fetch pod
	g.Go(func() error {
		var err error
		pod, err = v1.Pods(helmClient.TargetNamespace()).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("%w: %s", ErrWorkspaceNotFound, name)
			}
			if strings.Contains(err.Error(), "unable to parse") {
				return fmt.Errorf("%w: %s", ErrInvalidParameters, name)
			}
			return fmt.Errorf("failed to get workspace %s: %w", name, err)
		}
		return nil
	})

	// Fetch access keys secret
	g.Go(func() error {
		var err error
		keysSecret, err = v1.Secrets(helmClient.TargetNamespace()).Get(ctx, name+"-access-keys", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get keys %s: %w", name, err)
		}
		return nil
	})

	// Fetch TLS secret
	g.Go(func() error {
		var err error
		tlsSecret, err = v1.Secrets(helmClient.TargetNamespace()).Get(ctx, name+"-tls", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get tls secret %s: %w", name, err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	accessKey, ok := keysSecret.Data["a1key"]
	if !ok {
		return nil, fmt.Errorf("failed to get access key from keys %s", name)
	}

	tlsCert, ok := tlsSecret.Data["tls.crt"]
	if !ok {
		return nil, fmt.Errorf("failed to get tls cert from secret %s", name)
	}

	status := &models.WorkspaceStatus{
		Created:   pod.CreationTimestamp.Time,
		Status:    string(pod.Status.Phase),
		Host:      fmt.Sprintf("%s.%s", pod.ObjectMeta.Name, pod.ObjectMeta.Namespace),
		Message:   getPodStatusMessage(pod),
		AccessKey: string(accessKey),
		TLSCert:   string(tlsCert),
	}

	return status, nil
}

// GetSelector returns a label selector string from the given labels map
func GetSelector(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	var selectors []string
	for key, value := range labels {
		selectors = append(selectors, fmt.Sprintf("%s=%s", key, value))
	}

	return strings.Join(selectors, ",")
}

// *** Workspace methods

// NewWorkspace creates a new workspace with the specified Helm chart
func NewWorkspace(blueprint *models.Blueprint, user *identity.User, client *helm.Client) (*Workspace, error) {
	return &Workspace{
		log:       log.NewLogger("workspace"),
		client:    client,
		blueprint: blueprint,
		user:      user,
	}, nil
}

// Name returns the name of the workspace
func (w *Workspace) Name() string {
	return w.blueprint.Name + "-" + w.user.Username
}

func (w *Workspace) Labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       helm.WORKSPACE_CHART_NAME,
		"app.kubernetes.io/instance":   w.Name(),
		"app.kubernetes.io/version":    "1.0.0",
		"app.kubernetes.io/managed-by": "k8shell-provisioner",
		"k8shell.io/app":               helm.WORKSPACE_CHART_NAME,
		"k8shell.io/username":          w.user.Username,
		"k8shell.io/blueprint":         w.blueprint.Name,
		"k8shell.io/organization":      w.user.Organization,
	}
}

func (w *Workspace) getK8shelldVersion() string {
	if w.blueprint.K8shelld.Image != "" {
		parts := strings.Split(w.blueprint.K8shelld.Image, ":")
		if len(parts) >= 2 {
			tag := parts[len(parts)-1]
			return strings.TrimPrefix(tag, "v")
		}
	}
	return "1.0.0"
}

// Selector returns the label selector for the workspace used to identify the workspace in Kubernetes
// It uses the app.kubernetes.io/instance label to match the workspace name
func (w *Workspace) Selector() string {
	return fmt.Sprintf("app.kubernetes.io/instance=%s", w.Name())
}

func (w *Workspace) Values() (map[string]interface{}, error) {
	values, err := w.blueprint.Values()
	if err != nil {
		return nil, err
	}

	key, cert, err := w.generateKeyCert()
	if err != nil {
		return nil, fmt.Errorf("failed to generate key and certificate: %w", err)
	}

	a1key, a2key, err := w.generateAccessKeys()
	if err != nil {
		return nil, fmt.Errorf("failed to generate access keys: %w", err)
	}

	userValues, err := toMap(w.user)
	if err != nil {
		return nil, fmt.Errorf("failed to convert user to map: %w", err)
	}

	values["__user__"] = userValues
	values["__username__"] = w.user.Username
	values["__workspace__"] = w.Name()
	values["__blueprint__"] = w.blueprint.Name
	values["__organization__"] = w.user.Organization
	values["__tlscrt__"] = cert
	values["__tlskey__"] = key
	// values["__a1key__"] = base64.StdEncoding.EncodeToString([]byte(a1key))
	// values["__a2key__"] = base64.StdEncoding.EncodeToString([]byte(a2key))
	values["__a1key__"] = a1key
	values["__a2key__"] = a2key
	values["__registry__"] = w.client.Registry.ToValues()

	config, err := w.k8shelldConfig(w.blueprint.K8shelld.EncryptConfig, a1key, values)
	if err != nil {
		return nil, fmt.Errorf("failed to generate k8shelld config YAML: %w", err)
	}
	values["__k8shelldconfig__"] = config

	return values, nil
}

func (w *Workspace) Template(ctx context.Context) (string, error) {
	values, err := w.Values()
	if err != nil {
		return "", err
	}
	out, err := w.client.Template(ctx, helm.WORKSPACE_CHART_NAME, helm.InstallOptions{
		ReleaseName: w.blueprint.Name,
		Values:      values,
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// GetStatus returns the current status of the workspace pod
func (w *Workspace) GetStatus(ctx context.Context) (*models.WorkspaceStatus, error) {
	return GetWorkspaceStatus(ctx, w.client, w.Name())
}

func (w *Workspace) IsInstalled(ctx context.Context) (bool, error) {
	_, err := w.client.GetRelease(w.Name())
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "release: not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ToMap converts any struct to a map[string]interface{} representation
func toMap(b any) (map[string]interface{}, error) {
	yamlBytes, err := yaml.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal struct to YAML: %w", err)
	}

	var values map[string]interface{}
	if err := yaml.Unmarshal(yamlBytes, &values); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML to map: %w", err)
	}

	return values, nil
}

// *** helpers

// getPodStatusMessage extracts detailed status information from pod
func getPodStatusMessage(pod *corev1.Pod) string {
	phase := pod.Status.Phase

	switch phase {
	case corev1.PodPending:
		return getPendingMessage(pod)
	case corev1.PodRunning:
		return getRunningMessage(pod)
	case corev1.PodFailed:
		return getFailedMessage(pod)
	case corev1.PodSucceeded:
		return "Pod completed successfully"
	default:
		return string(phase)
	}
}

// getPendingMessage gets detailed message for pending pods
func getPendingMessage(pod *corev1.Pod) string {
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.State.Waiting != nil {
			waiting := containerStatus.State.Waiting
			switch waiting.Reason {
			case "ImagePullBackOff":
				return fmt.Sprintf("Image pull failed: %s", waiting.Message)
			case "ErrImagePull":
				return fmt.Sprintf("Error pulling image: %s", waiting.Message)
			case "ContainerCreating":
				return "Container is being created"
			case "PodInitializing":
				return "Pod is initializing"
			default:
				if waiting.Message != "" {
					return fmt.Sprintf("%s: %s", waiting.Reason, waiting.Message)
				}
				return waiting.Reason
			}
		}
	}

	// Check init container statuses
	for _, containerStatus := range pod.Status.InitContainerStatuses {
		if containerStatus.State.Waiting != nil {
			waiting := containerStatus.State.Waiting
			if waiting.Message != "" {
				return fmt.Sprintf("Init container %s: %s", waiting.Reason, waiting.Message)
			}
			return fmt.Sprintf("Init container: %s", waiting.Reason)
		}
	}

	// Check pod conditions for scheduling issues
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			return fmt.Sprintf("Scheduling failed: %s", condition.Message)
		}
		if condition.Status == corev1.ConditionFalse && condition.Message != "" {
			return fmt.Sprintf("%s: %s", condition.Type, condition.Message)
		}
	}

	// Fallback to pod status reason/message
	if pod.Status.Reason != "" {
		if pod.Status.Message != "" {
			return fmt.Sprintf("%s: %s", pod.Status.Reason, pod.Status.Message)
		}
		return pod.Status.Reason
	}

	return "Pod is pending"
}

// getRunningMessage gets message for running pods
func getRunningMessage(pod *corev1.Pod) string {
	// Check if all containers are ready
	readyCount := 0
	totalCount := len(pod.Status.ContainerStatuses)

	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Ready {
			readyCount++
		} else if containerStatus.State.Waiting != nil {
			waiting := containerStatus.State.Waiting
			return fmt.Sprintf("Container not ready: %s", waiting.Reason)
		} else if containerStatus.State.Terminated != nil {
			terminated := containerStatus.State.Terminated
			return fmt.Sprintf("Container terminated: %s", terminated.Reason)
		}
	}

	if readyCount == totalCount {
		return "All containers are ready"
	}

	return fmt.Sprintf("Containers ready: %d/%d", readyCount, totalCount)
}

// getFailedMessage gets message for failed pods
func getFailedMessage(pod *corev1.Pod) string {
	// Check container statuses for failure details
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.State.Terminated != nil {
			terminated := containerStatus.State.Terminated
			if terminated.Message != "" {
				return fmt.Sprintf("Container failed: %s - %s", terminated.Reason, terminated.Message)
			}
			return fmt.Sprintf("Container failed: %s (exit code: %d)", terminated.Reason, terminated.ExitCode)
		}
	}

	// Check pod conditions
	for _, condition := range pod.Status.Conditions {
		if condition.Status == corev1.ConditionFalse && condition.Message != "" {
			return condition.Message
		}
	}

	if pod.Status.Message != "" {
		return pod.Status.Message
	}

	return "Pod failed"
}
