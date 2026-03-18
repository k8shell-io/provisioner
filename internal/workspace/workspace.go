package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/gapi"
	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/common/pkg/models"
	identity "github.com/k8shell-io/identity/pkg/api"
	"github.com/k8shell-io/identity/pkg/api/typespb"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Default page size for GetWorkspaces pagination when limit is not specified or invalid
const WORKSPACE_DEFAULT_PAGE_SIZE = 20

// Workspace represents a workspace with Helm client
type Workspace struct {
	config   *config.Config
	client   *helm.Client
	identify *identity.IdentityClient

	Name          string
	JobId         string
	log           *zerolog.Logger
	blueprint     *models.Blueprint
	user          *models.User
	userStr       *models.CanonicalUserStr
	workspaceLock *WorkspaceLock
}

type Values struct {
	Values map[string]interface{}
}

// GetWorkspacesOptions defines the options for retrieving workspaces with filtering
type GetWorkspacesOptions struct {
	Username     string
	Organization string
	Blueprint    string
	Workspace    string
	RepoName     string
	RepoOwner    string
	RepoRef      string
}

// GetWorkspacesResult defines the result structure for GetWorkspaces function,
// including the list of workspaces and pagination token
type GetWorkspacesResult struct {
	Workspaces []*models.WorkspaceDetails // list of workspaces matching the filters and pagination
	Pods       []corev1.Pod               // corresponding pods for the workspaces, used for internal processing
}

// WorkspaceLabels holds parsed workspace metadata stored in k8shell labels.
type WorkspaceLabels struct {
	Workspace    string
	Username     string
	Organization string
	Blueprint    string
	RepoOwner    string
	RepoName     string
	RepoRef      string
	AppVersion   string
	UserStr      *models.CanonicalUserStr
	JobId        string
}

// ParseWorkspaceLabels parses the label set attached to a workspace pod
func ParseWorkspaceLabels(labels map[string]string) (*WorkspaceLabels, error) {
	if labels == nil {
		return nil, fmt.Errorf("workspace labels are nil")
	}

	username, ok := labels["k8shell.io/username"]
	if !ok || username == "" {
		return nil, fmt.Errorf("missing label k8shell.io/username")
	}

	blueprint, ok := labels["k8shell.io/blueprint"]
	if !ok || blueprint == "" {
		return nil, fmt.Errorf("missing label k8shell.io/blueprint")
	}

	userstrB64, ok := labels["k8shell.io/userstr"]
	if !ok || userstrB64 == "" {
		return nil, fmt.Errorf("missing label k8shell.io/userstr")
	}

	canUser, err := models.NewCanonicalUserStrFromBase64(userstrB64)
	if err != nil {
		return nil, fmt.Errorf("parse k8shell.io/userstr: %w", err)
	}

	return &WorkspaceLabels{
		Workspace:    labels["k8shell.io/workspace"],
		Username:     username,
		Organization: labels["k8shell.io/organization"],
		Blueprint:    blueprint,
		RepoOwner:    labels["k8shell.io/repo-owner"],
		RepoName:     labels["k8shell.io/repo-name"],
		RepoRef:      labels["k8shell.io/repo-ref"],
		AppVersion:   labels["app.kubernetes.io/version"],
		JobId:        labels["k8shell.io/job-id"],
		UserStr:      canUser,
	}, nil
}

// FindWorkspace finds a workspace by name and returns its status
func FindWorkspace(ctx context.Context, helmClient *helm.Client, workspace string) (*models.WorkspaceDetails,
	*corev1.Pod, error) {

	ws, err := GetWorkspaces(ctx, helmClient, GetWorkspacesOptions{
		Workspace: workspace,
	})
	if err != nil {
		return nil, nil, err
	}
	if len(ws.Workspaces) == 0 {
		return nil, nil, fmt.Errorf("%w: %s", models.ErrWorkspaceNotFound, workspace)
	}
	if len(ws.Workspaces) > 1 {
		return nil, nil, fmt.Errorf("multiple workspaces found with name %s", workspace)
	}
	return ws.Workspaces[0], &ws.Pods[0], nil
}

// GetWorkspaces lists workspace pods matching optional filters and returns status details
// similar to GetWorkspaceStatus, without fetching Service/Secret per workspace.
func GetWorkspaces(ctx context.Context, helmClient *helm.Client,
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
	if opts.RepoName != "" {
		labels["k8shell.io/repo-name"] = opts.RepoName
	}
	if opts.RepoOwner != "" {
		labels["k8shell.io/repo-owner"] = opts.RepoOwner
	}
	if opts.RepoRef != "" {
		labels["k8shell.io/repo-ref"] = opts.RepoRef
	}

	selector := getSelector(labels)
	v1 := helmClient.KubeClient().CoreV1()
	namespace := helmClient.TargetNamespace()

	podList, err := v1.Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		if strings.Contains(err.Error(), "unable to parse") {
			return nil, fmt.Errorf("%w: %s", models.ErrInvalidParameters, selector)
		}
		return nil, fmt.Errorf("failed to list workspace pods: %w", err)
	}

	out := make([]*models.WorkspaceDetails, 0, len(podList.Items))
	for i := range podList.Items {
		p := &podList.Items[i]
		d := WorkspaceDetailsFromPod(p)
		if d == nil {
			continue
		}
		out = append(out, d)
	}

	return &GetWorkspacesResult{
		Workspaces: out,
		Pods:       podList.Items,
	}, nil
}

// FindworkspaceByName finds a workspace by its name using Helm client and returns the corresponding release
func FindWorkspaceHelmRelease(_ context.Context, helmClient *helm.Client, name string) (*release.Release, error) {
	labels := map[string]string{
		"app.kubernetes.io/name":     helm.WORKSPACE_CHART_NAME,
		"app.kubernetes.io/instance": name,
	}

	selector := getSelector(labels)
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
	identityClient *identity.IdentityClient,
	config *config.Config,
) (*Workspace, error) {

	return &Workspace{
		Name:      workspaceName,
		log:       log.NewLogger("workspace"),
		client:    helmClient,
		identify:  identityClient,
		blueprint: blueprint,
		config:    config,
		user:      user,
		userStr:   userStr,
	}, nil
}

// NewWorkspaceFromHelmRelease creates a workspace instance from an existing Helm release
func NewWorkspaceFromHelmRelease(ctx context.Context, name string, helmClient *helm.Client,
	identityClient *identity.IdentityClient, config *config.Config) (*Workspace, error) {

	release, err := FindWorkspaceHelmRelease(ctx, helmClient, name)
	if err != nil {
		return nil, err
	}
	username := release.Labels["k8shell.io/username"]
	blueprintName := release.Labels["k8shell.io/blueprint"]

	userpb, err := identityClient.FindUser(ctx, &typespb.FindUserRequest{Username: username})
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
		Name:      name,
		log:       log.NewLogger("workspace"),
		client:    helmClient,
		identify:  identityClient,
		blueprint: blueprint,
		user:      user,
		config:    config,
	}

	return ws, nil
}

func (w *Workspace) SetJobId(jobId string) {
	w.JobId = jobId
}

func (w *Workspace) CreateLock() *WorkspaceLock {
	return NewWorkspaceLock(
		w.client.KubeClient(),
		w.client.TargetNamespace(),
		w.Name,
	)
}

func (w *Workspace) appVersion() string {
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
	if w.blueprint == nil {
		return nil, fmt.Errorf("blueprint is nil for workspace %s", w.Name)
	}

	values, err := toMap(w.blueprint)
	if err != nil {
		return nil, fmt.Errorf("failed to convert blueprint to map: %w", err)
	}

	userValues, err := toMap(w.user)
	if err != nil {
		return nil, fmt.Errorf("failed to convert user to map: %w", err)
	}

	cmValues, err := toMap(w.config.CertManager)
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
	values["__appversion__"] = w.appVersion()
	values["__identity__"] = w.user.Source
	values["__userstr__"] = userstrB64
	values["__jobid__"] = w.JobId
	values["__apiserver__"] = map[string]interface{}{
		"enabled": w.config.K8shellCapabilities.APIServerEnabled,
	}
	values["__identityPublicKey__"] = w.config.IdentityVerifier.PublicKeyFile != ""
	values["__identityVerifier__"] = map[string]interface{}{ // #nosec G101 -- these are file paths, not credentials
		"tokenPath":     "/run/secrets/identity-token/token",
		"publicKeyPath": "/run/secrets/identity-public-key/identity_public.pem",
		"signingMethod": w.config.IdentityVerifier.SigningMethod,
	}

	return values, nil
}

func (w *Workspace) Template(ctx context.Context) (string, error) {
	values, err := w.Values()
	if err != nil {
		return "", err
	}

	values["__jobid__"] = ""
	values["__manifesthash__"] = ""

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
		Created:         pod.CreationTimestamp.Time,
		Status:          workspacePodStatus(pod),
		Message:         workspacePodMessage(pod),
		Restarts:        podRestartCount(pod),
		LastFailMessage: podLastFailure(pod),
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

// workspacePodStatus extracts the workspace details from pod
func WorkspaceDetailsFromPod(pod *corev1.Pod) *models.WorkspaceDetails {
	if pod == nil {
		return nil
	}

	if len(pod.Spec.Containers) == 0 {
		return nil
	}

	var splash string
	if splashAnnotation, exists := pod.Annotations["workspace.k8shell.io/splash"]; exists {
		if decoded, derr := base64.StdEncoding.DecodeString(splashAnnotation); derr == nil {
			splash = string(decoded)
		}
	}

	appVersion, exists := pod.Labels["app.kubernetes.io/version"]
	if !exists {
		appVersion = "1.0.0"
	}

	serverName := pod.Name + "." + pod.Namespace
	port := getPodContainerPort(pod, models.WORKSPACE_PORT)
	tlsEnabled := podMountsSecret(pod, pod.Name+"-tls")

	nameLabel := pod.Labels["k8shell.io/workspace"]
	if nameLabel == "" {
		nameLabel = pod.Name
	}

	cpu := pod.Spec.Containers[0].Resources.Limits.Cpu().String()
	memory := pod.Spec.Containers[0].Resources.Limits.Memory().String()

	wsDetails := &models.WorkspaceDetails{
		PodStatus: models.PodStatus{
			Created:         pod.CreationTimestamp.Time,
			Status:          workspacePodStatus(pod),
			Message:         workspacePodMessage(pod),
			Restarts:        podRestartCount(pod),
			LastFailMessage: podLastFailure(pod),
		},
		Name:         nameLabel,
		Username:     pod.Labels["k8shell.io/username"],
		RepoOwner:    pod.Labels["k8shell.io/repo-owner"],
		RepoName:     pod.Labels["k8shell.io/repo-name"],
		RepoRef:      pod.Labels["k8shell.io/repo-ref"],
		Blueprint:    pod.Labels["k8shell.io/blueprint"],
		Organization: pod.Labels["k8shell.io/organization"],
		JobId:        pod.Labels["k8shell.io/job-id"],
		ServerName:   serverName,
		PodIP:        pod.Status.PodIP,
		Port:         port,
		TLSEnabled:   tlsEnabled,
		Splash:       splash,
		AppVersion:   appVersion,
		CPU:          cpu,
		Memory:       memory,
		Hostname:     podHostname(pod),
	}

	return wsDetails
}
