package workspace

import (
	"context"
	"fmt"
	"strings"
	"time"

	identity "github.com/k8shell-io/identity/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/k8shell-io/provisioner/pkg/models"
	"github.com/rs/zerolog"
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

// WorkspaceStatus represents the current status of a workspace
type WorkspaceStatus struct {
	Created       time.Time
	Status        string
	Message       string
	PodIP         string
	ProvisionTime time.Duration
}

// NewWorkspace creates a new workspace with the specified Helm chart
func NewWorkspace(blueprint *models.Blueprint, user *identity.User,
	client *helm.Client) (*Workspace, error) {
	return &Workspace{
		log:       log.NewLogger("workspace"),
		client:    client,
		blueprint: blueprint,
		user:      user,
	}, nil
}

func (w *Workspace) Name() string {
	return w.blueprint.Name + "-" + w.user.Username
}

// Namespace returns the namespace where the workspace is deployed
func (w *Workspace) Namespace() string {
	return fmt.Sprintf("workspaces-%s", w.user.Organization)
}

func (w *Workspace) Labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "k8shell-workspace",
		"app.kubernetes.io/instance":   w.Name(),
		"app.kubernetes.io/version":    "1.0.0",
		"app.kubernetes.io/managed-by": "k8shell-provisioner",
		"k8shell.io/app":               "k8shell-workspace",
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

	key, cert, err := w.generateSelfSignedCert(w.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to generate TLS certificate: %w", err)
	}

	a1key, a2key, err := w.generateAccessKeys()
	if err != nil {
		return nil, fmt.Errorf("failed to generate access keys: %w", err)
	}

	userValues, err := ToMap(w.user)
	if err != nil {
		return nil, fmt.Errorf("failed to convert user to map: %w", err)
	}

	values["__user__"] = userValues
	values["__namespace__"] = w.Namespace()
	values["__workspace__"] = w.Name()
	values["__blueprint__"] = w.blueprint.Name
	values["__username__"] = w.user.Username
	values["__organization__"] = w.user.Organization
	values["__tlscrt__"] = cert
	values["__tlskey__"] = key
	values["__a1key__"] = a1key
	values["__a2key__"] = a2key

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
		Namespace:   w.Namespace(),
		Values:      values,
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// GetStatus returns the current status of the workspace pod
func (w *Workspace) GetStatus(ctx context.Context) (*WorkspaceStatus, error) {
	podName := w.Name()

	pod, err := w.client.GetKubeClient().CoreV1().Pods(w.Namespace()).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return &WorkspaceStatus{
			Status: "NotFound",
		}, nil
	}

	status := &WorkspaceStatus{
		Created: pod.CreationTimestamp.Time,
		Status:  string(pod.Status.Phase),
		PodIP:   pod.Status.PodIP,
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Status == corev1.ConditionFalse && condition.Message != "" {
			status.Message = condition.Message
			break
		}
	}

	return status, nil
}

func (w *Workspace) IsInstalled(ctx context.Context) (bool, error) {
	_, err := w.client.GetRelease(w.Name(), w.Namespace())
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "release: not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ToMap converts any struct to a map[string]interface{} representation
func ToMap(b any) (map[string]interface{}, error) {
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
