package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/gapi"
	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/common/pkg/models"
	identity "github.com/k8shell-io/identity/pkg/api"
	"github.com/k8shell-io/identity/pkg/api/identitypb"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Default page size for GetWorkspaces pagination when limit is not specified or invalid
const WORKSPACE_DEFAULT_PAGE_SIZE = 20

// Workspace represents a workspace with Helm client
type Workspace struct {
	Name          string
	log           *zerolog.Logger
	client        *helm.Client
	identify      *identity.Client
	blueprint     *models.Blueprint
	user          *models.User
	userStr       *models.CanonicalUserStr
	certManager   *config.CertManagerConfig
	caps          *config.K8shellCapabilities
	workspaceLock *WorkspaceLock
}

type Values struct {
	Values map[string]interface{}
}

// GetWorkspacesOptions defines the options for retrieving workspaces with filtering and pagination
type GetWorkspacesOptions struct {
	Username     string // filters for workspaces based on labels; empty means no filtering
	Organization string // filters for workspaces based on labels; empty means no filtering
	Blueprint    string // filters for workspaces based on labels; empty means no filtering
	Workspace    string // filters for workspaces based on labels; empty means no filtering
}

// GetWorkspacesResult defines the result structure for GetWorkspaces function,
// including the list of workspaces and pagination token
type GetWorkspacesResult struct {
	Workspaces []*models.WorkspaceStatus // list of workspaces matching the filters and pagination
	Continue   string                    // token for next page; empty when no more pages
}

// FindWorkspace finds a workspace by name and returns its status
func FindWorkspace(ctx context.Context, v1 typedcorev1.CoreV1Interface, namespace string,
	workspace string) (*models.WorkspaceStatus, error) {
	ws, err := GetWorkspaces(ctx, v1, namespace, GetWorkspacesOptions{
		Workspace: workspace,
	})
	if err != nil {
		return nil, err
	}
	if len(ws.Workspaces) == 0 {
		return nil, fmt.Errorf("%w: %s", models.ErrWorkspaceNotFound, workspace)
	}
	if len(ws.Workspaces) > 1 {
		return nil, fmt.Errorf("multiple workspaces found with name %s", workspace)
	}
	return ws.Workspaces[0], nil
}

// GetWorkspaces lists workspace pods matching optional filters and returns status details
// similar to GetWorkspaceStatus, without fetching Service/Secret per workspace.
func GetWorkspaces(ctx context.Context, v1 typedcorev1.CoreV1Interface, namespace string,
	opts GetWorkspacesOptions) (*GetWorkspacesResult, error) {
	labels := map[string]string{}
	labels["k8shell.io/app"] = "k8shell-workspace"

	if opts.Username != "" {
		labels["k8shell.io/username"] = opts.Username
	}
	if opts.Workspace != "" {
		labels["k8shell.io/workspace"] = opts.Workspace
	}
	if opts.Organization != "" {
		labels["k8shell.io/organization"] = opts.Organization
	}
	if opts.Blueprint != "" {
		labels["k8shell.io/blueprint"] = opts.Blueprint
	}

	selector := GetSelector(labels)

	podList, err := v1.Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		if strings.Contains(err.Error(), "unable to parse") {
			return nil, fmt.Errorf("%w: %s", models.ErrInvalidParameters, selector)
		}
		return nil, fmt.Errorf("failed to list workspace pods: %w", err)
	}

	out := make([]*models.WorkspaceStatus, 0, len(podList.Items))
	for i := range podList.Items {
		p := &podList.Items[i]

		var splash string
		if splashAnnotation, exists := p.Annotations["workspace.k8shell.io/splash"]; exists {
			if decoded, derr := base64.StdEncoding.DecodeString(splashAnnotation); derr == nil {
				splash = string(decoded)
			}
		}

		appVersion, exists := p.Labels["app.kubernetes.io/version"]
		if !exists {
			appVersion = "1.0.0"
		}

		host := p.Name + "." + p.Namespace
		port := getPodContainerPort(p, models.WORKSPACE_PORT)
		tlsEnabled := podMountsSecret(p, p.Name+"-tls")

		nameLabel := p.Labels["k8shell.io/workspace"]
		if nameLabel == "" {
			nameLabel = p.Name
		}

		out = append(out, &models.WorkspaceStatus{
			PodStatus: models.PodStatus{
				Created: p.CreationTimestamp.Time,
				Status:  string(p.Status.Phase),
				Message: getPodStatusMessage(p),
			},
			Name:         nameLabel,
			Username:     p.Labels["k8shell.io/username"],
			RepoOwner:    p.Labels["k8shell.io/repo-owner"],
			RepoName:     p.Labels["k8shell.io/repo-name"],
			RepoRef:      p.Labels["k8shell.io/repo-ref"],
			Blueprint:    p.Labels["k8shell.io/blueprint"],
			Organization: p.Labels["k8shell.io/organization"],
			Host:         host,
			PodIP:        p.Status.PodIP,
			Port:         port,
			TLSEnabled:   tlsEnabled,
			Splash:       splash,
			AppVersion:   appVersion,
		})
	}

	return &GetWorkspacesResult{
		Workspaces: out,
		Continue:   podList.Continue,
	}, nil
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

// FindworkspaceByName finds a workspace by its name using Helm client and returns the corresponding release
func FindworkspaceByName(ctx context.Context, helmClient *helm.Client, name string) (*release.Release, error) {
	labels := map[string]string{
		"app.kubernetes.io/name":     helm.WORKSPACE_CHART_NAME,
		"app.kubernetes.io/instance": name,
	}

	selector := GetSelector(labels)
	releases, err := helmClient.ListWithSelector(helmClient.TargetNamespace(), selector)
	if err != nil {
		if strings.Contains(err.Error(), "unable to parse") {
			return nil, fmt.Errorf("failed to list releases: %w", models.ErrInvalidParameters)
		}
		return nil, fmt.Errorf("failed to list releases: %w", err)
	}

	if len(releases) == 0 {
		return nil, fmt.Errorf("%w: %s", models.ErrWorkspaceNotFound, name)
	}
	if len(releases) > 1 {
		return nil, fmt.Errorf("multiple releases found for workspace %s", name)
	}

	return releases[0], nil
}

// *** Workspace methods

// NewWorkspace creates a new workspace with the specified Helm chart
func NewWorkspace(
	workspaceName string,
	blueprint *models.Blueprint,
	user *models.User,
	userStr *models.CanonicalUserStr,
	helmClient *helm.Client,
	identityClient *identity.Client,
	certManager *config.CertManagerConfig,
	caps *config.K8shellCapabilities,
) (*Workspace, error) {

	return &Workspace{
		Name:        workspaceName,
		log:         log.NewLogger("workspace"),
		client:      helmClient,
		identify:    identityClient,
		blueprint:   blueprint,
		certManager: certManager,
		caps:        caps,
		user:        user,
		userStr:     userStr,
	}, nil
}

// NewWorkspaceFromHelmRelease creates a workspace instance from an existing Helm release
func NewWorkspaceFromHelmRelease(ctx context.Context, name string, helmClient *helm.Client,
	identityClient *identity.Client, certManager *config.CertManagerConfig,
	caps *config.K8shellCapabilities) (*Workspace, error) {

	release, err := FindworkspaceByName(ctx, helmClient, name)
	if err != nil {
		return nil, err
	}
	username := release.Labels["k8shell.io/username"]
	blueprintName := release.Labels["k8shell.io/blueprint"]

	userpb, err := identityClient.FindUser(ctx, &identitypb.FindUserRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("failed to get user %s: %w", username, err)
	}
	user := gapi.ProtoToUser(userpb)

	values := release.Config
	blueprint := &models.Blueprint{}
	yamlBytes, err := yaml.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal blueprint values: %w", err)
	}
	if err := yaml.Unmarshal(yamlBytes, blueprint); err != nil {
		return nil, fmt.Errorf("failed to unmarshal blueprint values: %w", err)
	}
	blueprint.Name = blueprintName

	ws := &Workspace{
		Name:        name,
		log:         log.NewLogger("workspace"),
		client:      helmClient,
		identify:    identityClient,
		blueprint:   blueprint,
		user:        user,
		certManager: certManager,
		caps:        caps,
	}

	return ws, nil
}

func (w *Workspace) CreateLock() *WorkspaceLock {
	return NewWorkspaceLock(
		w.client.KubeClient(),
		w.client.TargetNamespace(),
		w.Name,
	)
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
	return fmt.Sprintf("app.kubernetes.io/instance=%s", w.Name)
}

func (w *Workspace) Values() (map[string]interface{}, error) {
	values, err := toMap(w.blueprint)
	if err != nil {
		return nil, fmt.Errorf("failed to convert blueprint to map: %w", err)
	}

	userValues, err := toMap(w.user)
	if err != nil {
		return nil, fmt.Errorf("failed to convert user to map: %w", err)
	}

	cmValues, err := toMap(w.certManager)
	if err != nil {
		return nil, fmt.Errorf("failed to convert cert manager config to map: %w", err)
	}

	userstrB64 := ""
	if w.userStr != nil {
		userstrB64 = w.userStr.Base64()
	}

	values["__user__"] = userValues
	values["__username__"] = w.user.Username
	values["__repoowner__"] = w.userStr.Identity.RepoOwner
	values["__reponame__"] = w.userStr.Identity.RepoName
	values["__reporef__"] = w.userStr.Identity.RepoRef
	values["__workspace__"] = w.Name
	values["__blueprint__"] = w.blueprint.Name
	values["__organization__"] = w.user.Organization
	values["__registry__"] = w.client.Registry.ToValues()
	values["__namespace__"] = getNamespace()
	values["__certmanager__"] = cmValues
	values["__appversion__"] = w.getK8shelldVersion()
	values["__identity__"] = w.user.Source
	values["__userstr__"] = userstrB64
	values["__apiserver__"] = map[string]interface{}{
		"enabled": w.caps.APIServerEnabled,
	}
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

func (w *Workspace) TemplateHash(ctx context.Context) (string, error) {
	template, err := w.Template(ctx)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	h.Write([]byte(template))
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (w *Workspace) GetPodStatus(ctx context.Context) (*models.PodStatus, error) {
	v1 := w.client.KubeClient().CoreV1()
	pod, err := v1.Pods(w.client.TargetNamespace()).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", models.ErrWorkspaceNotFound, w.Name)
		}
		return nil, fmt.Errorf("failed to get workspace pod status %s: %w", w.Name, err)
	}
	return &models.PodStatus{
		Created: pod.CreationTimestamp.Time,
		Status:  string(pod.Status.Phase),
		Message: getPodStatusMessage(pod),
	}, nil
}

func (w *Workspace) IsInstalled(ctx context.Context) (bool, error) {
	_, err := w.client.GetRelease(w.Name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "release: not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (w *Workspace) Uninstall(ctx context.Context, timeout time.Duration, wait bool, lock bool) error {
	if lock {
		err := w.lock(timeout)
		if err != nil {
			return fmt.Errorf("failed to acquire lock: %w", err)
		}
		defer func() {
			if releaseErr := w.unlock(); releaseErr != nil {
				w.log.Error().Err(releaseErr).Msgf("Failed to release lock for workspace %s", w.Name)
			}
		}()
	}

	if err := w.client.Uninstall(w.Name, int(timeout.Seconds()), wait); err != nil {
		return fmt.Errorf("failed to uninstall workspace: %w", err)
	}
	return nil
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
		return "Workspace is ready"
	}

	return fmt.Sprintf("Containers ready: %d/%d", readyCount, totalCount)
}

// getFailedMessage gets message for failed pods
func getFailedMessage(pod *corev1.Pod) string {
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

func getNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return string(data)
	// return "k8shell-test"
}

func getPodContainerPort(pod *corev1.Pod, defaultPort int) int {
	preferredNames := map[string]struct{}{
		"grpc":  {},
		"https": {},
		"http":  {},
	}

	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.ContainerPort <= 0 {
				continue
			}
			if _, ok := preferredNames[strings.ToLower(p.Name)]; ok {
				return int(p.ContainerPort)
			}
		}
	}

	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.ContainerPort > 0 {
				return int(p.ContainerPort)
			}
		}
	}

	return defaultPort
}

func podMountsSecret(pod *corev1.Pod, secretName string) bool {
	if secretName == "" {
		return false
	}
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == secretName {
			return true
		}
	}
	return false
}
