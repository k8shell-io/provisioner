package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/identity"
	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/userstr"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	Name           string
	JobId          string
	log            *zerolog.Logger
	blueprint      *models.Blueprint
	blueprintChain []string // ordered inheritance chain from root ancestor to this blueprint
	user           *models.User
	userStr        *userstr.CanonicalUserStr
	workspaceLock  *WorkspaceLock
}

type Values struct {
	Values map[string]interface{}
}

// GetWorkspacesOptions defines the options for retrieving workspaces with filtering
type GetWorkspacesOptions struct {
	Username        string
	Organization    string
	Blueprint       string
	WorkspaceName   string
	TargetNamespace string
	CanonicalId     string
	// InjectNamespaces controls where injected workspaces are discovered.
	// Use "*" for cluster-wide discovery.
	InjectNamespaces []string
	// InjectWorkload and InjectKind filter injected workspaces by the workload they
	// were injected into. InjectKind is optional; omitting it matches any kind.
	InjectWorkload string
	InjectKind     string
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
	UserStr      *userstr.CanonicalUserStr
	JobId        string
}

func canonicalUserStrToBase64(c *userstr.CanonicalUserStr) string {
	if c == nil || c.CanonicalUserStr() == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(c.CanonicalUserStr()))
}

func parseCanonicalUserStrFromBase64(s string) (*userstr.CanonicalUserStr, error) {
	if s == "" {
		return nil, fmt.Errorf("empty userstr")
	}

	var (
		raw *userstr.UserStr
		err error
	)

	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "b64-") || strings.HasPrefix(lower, "base64-") {
		raw, err = userstr.ParseUserStr(s)
	} else {
		decoded, derr := base64.RawURLEncoding.DecodeString(s)
		if derr != nil {
			return nil, fmt.Errorf("base64 decode failed: %w", derr)
		}
		raw, err = userstr.ParseUserStr(string(decoded))
	}
	if err != nil {
		return nil, err
	}

	return raw.Canonicalize()
}

// FindWorkspace finds a workspace by name and returns its status
func FindWorkspace(ctx context.Context, helmClient *helm.Client, workspace string, injectionNamespaces []string) (*models.WorkspaceDetails,
	*corev1.Pod, error) {

	ws, err := GetWorkspaces(ctx, helmClient, GetWorkspacesOptions{
		WorkspaceName:    workspace,
		InjectNamespaces: injectionNamespaces,
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

func podMatchesWorkspaceFilters(p *corev1.Pod, opts GetWorkspacesOptions, injected bool) bool {
	if p == nil {
		return false
	}

	if opts.Username != "" && p.Labels[helm.LabelUsername] != opts.Username {
		return false
	}
	if opts.Organization != "" && p.Labels[helm.LabelOrganization] != opts.Organization {
		return false
	}
	if opts.Blueprint != "" && p.Labels[helm.LabelBlueprint] != opts.Blueprint {
		return false
	}

	return true
}

func GetWorkspaces(
	ctx context.Context,
	helmClient *helm.Client,
	opts GetWorkspacesOptions,
) (*GetWorkspacesResult, error) {
	if opts.InjectKind != "" && opts.InjectWorkload == "" {
		return nil, fmt.Errorf("%w: InjectKind specified without InjectWorkload", models.ErrInvalidParameters)
	}
	if opts.InjectKind == "" && opts.InjectWorkload != "" {
		return nil, fmt.Errorf("%w: InjectWorkload specified without InjectKind", models.ErrInvalidParameters)
	}

	targetNamespace := opts.TargetNamespace
	if targetNamespace == "" {
		targetNamespace = helmClient.TargetNamespace()
	}

	v1 := helmClient.KubeClient().CoreV1()
	out := make([]*models.WorkspaceDetails, 0)
	pods := make([]corev1.Pod, 0)

	if opts.WorkspaceName != "" && opts.InjectWorkload == "" {
		p, err := v1.Pods(targetNamespace).Get(ctx, opts.WorkspaceName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get workspace pod %q: %w", opts.WorkspaceName, err)
		}
		if err == nil && podMatchesWorkspaceFilters(p, opts, false) {
			if d := workspaceDetailsFromPod(p); d != nil {
				out = append(out, d)
				pods = append(pods, *p)
			}
		}
	} else if opts.InjectWorkload == "" {
		labels := map[string]string{}
		if opts.Username != "" {
			labels[helm.LabelUsername] = opts.Username
		}
		if opts.Organization != "" {
			labels[helm.LabelOrganization] = opts.Organization
		}
		if opts.Blueprint != "" {
			labels[helm.LabelBlueprint] = opts.Blueprint
		}
		if opts.CanonicalId != "" {
			labels[helm.LabelCanonicalId] = opts.CanonicalId
		}
		selector := getSelector(labels)

		podList, err := v1.Pods(targetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			if strings.Contains(err.Error(), "unable to parse") {
				return nil, fmt.Errorf("%w: %s", models.ErrInvalidParameters, selector)
			}
			return nil, fmt.Errorf("failed to list workspace pods: %w", err)
		}

		for i := range podList.Items {
			p := &podList.Items[i]
			if !podMatchesWorkspaceFilters(p, opts, false) {
				continue
			}
			d := workspaceDetailsFromPod(p)
			if d == nil {
				continue
			}
			out = append(out, d)
			pods = append(pods, *p)
		}
	}

	if len(opts.InjectNamespaces) > 0 {
		injectedLabels := map[string]string{
			helm.LabelInjected: "true",
		}
		if opts.Username != "" {
			injectedLabels[helm.LabelUsername] = opts.Username
		}
		if opts.Organization != "" {
			injectedLabels[helm.LabelOrganization] = opts.Organization
		}
		if opts.Blueprint != "" {
			injectedLabels[helm.LabelBlueprint] = opts.Blueprint
		}
		if opts.CanonicalId != "" {
			injectedLabels[helm.LabelCanonicalId] = opts.CanonicalId
		}
		if opts.InjectWorkload != "" {
			injectedLabels[helm.LabelWorkloadName] = strings.ToLower(opts.InjectWorkload)
			injectedLabels[helm.LabelWorkloadKind] = opts.InjectKind
		}
		injectedSelector := getSelector(injectedLabels)

		injectedItems := make([]corev1.Pod, 0)
		if opts.InjectNamespaces[0] == "*" {
			injectedList, err := v1.Pods("").List(ctx, metav1.ListOptions{
				LabelSelector: injectedSelector,
			})
			if err != nil {
				if strings.Contains(err.Error(), "unable to parse") {
					return nil, fmt.Errorf("%w: %s", models.ErrInvalidParameters, injectedSelector)
				}
				return nil, fmt.Errorf("failed to list injected workspace pods cluster-wide: %w", err)
			}
			injectedItems = append(injectedItems, injectedList.Items...)
		} else {
			listCtx, cancelLists := context.WithCancel(ctx)
			defer cancelLists()

			resultsCh := make(chan []corev1.Pod, len(opts.InjectNamespaces))
			errCh := make(chan error, 1)
			var wg sync.WaitGroup
			var once sync.Once

			for _, ns := range opts.InjectNamespaces {
				wg.Add(1)
				go func() {
					defer wg.Done()

					injectedList, err := v1.Pods(ns).List(listCtx, metav1.ListOptions{
						LabelSelector: injectedSelector,
					})
					if err != nil {
						wrappedErr := fmt.Errorf("failed to list injected workspace pods in namespace %q: %w", ns, err)
						if strings.Contains(err.Error(), "unable to parse") {
							wrappedErr = fmt.Errorf("%w: %s", models.ErrInvalidParameters, injectedSelector)
						}
						once.Do(func() {
							errCh <- wrappedErr
							cancelLists()
						})
						return
					}

					select {
					case resultsCh <- injectedList.Items:
					case <-listCtx.Done():
					}
				}()
			}

			wg.Wait()
			close(resultsCh)

			select {
			case err := <-errCh:
				return nil, err
			default:
			}

			for items := range resultsCh {
				injectedItems = append(injectedItems, items...)
			}
		}

		if len(injectedItems) > 0 {
			for i := range injectedItems {
				ip := &injectedItems[i]
				if opts.WorkspaceName != "" && ip.Name != opts.WorkspaceName {
					continue
				}

				d := workspaceDetailsFromPod(ip)
				if d == nil {
					continue
				}

				out = append(out, d)
				pods = append(pods, *ip)
			}
		}
	}

	return &GetWorkspacesResult{
		Workspaces: out,
		Pods:       pods,
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

// NewWorkspaceForEject creates a minimal workspace object sufficient to call Eject.
// Only the workspace name and helm client are required; no user, blueprint, or identity needed.
func NewWorkspaceForEject(name string, helmClient *helm.Client) (*Workspace, error) {
	if name == "" {
		return nil, fmt.Errorf("workspace name is required")
	}
	return &Workspace{
		Name:   name,
		log:    log.NewLogger("workspace"),
		client: helmClient,
	}, nil
}

// NewWorkspace creates a new workspace with the specified Helm chart
func NewWorkspace(
	workspaceName string,
	blueprint *models.Blueprint,
	user *models.User,
	userStr *userstr.CanonicalUserStr,
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
	username := release.Labels[helm.LabelUsername]
	blueprintName := release.Labels[helm.LabelBlueprint]

	userpb, err := identityClient.FindUser(ctx, &identityv1.FindUserRequest{Username: username})
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

// SetBlueprintChain stores the inheritance chain for this workspace's blueprint.
func (w *Workspace) SetBlueprintChain(chain []string) {
	w.blueprintChain = chain
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
		userstrB64 = canonicalUserStrToBase64(w.userStr)
	}

	values["__canonicalid__"] = w.userStr.CanonicalId()
	values["__user__"] = userValues
	values["__username__"] = w.user.Username
	values["__blueprint__"] = w.blueprint.Name
	values["__organization__"] = w.user.Organization
	values["__networkpolicy__"] = w.blueprint.Network.NetworkPolicyClass
	values["__registry__"] = w.client.Registry.ToValues()
	values["__jwtverifierpublickey__"] = w.client.JWTVerifierPublicKey
	values["__jwtverifiersigningmethod__"] = w.config.JWTVerifier.SigningMethod
	values["__namespace__"] = getNamespace()
	values["__certmanager__"] = cmValues
	values["__appversion__"] = w.appVersion()
	values["__identity__"] = w.user.Source
	values["__userstr__"] = userstrB64
	values["__jobid__"] = w.JobId

	configYAML, err := w.buildConfigYAML("")
	if err != nil {
		return nil, fmt.Errorf("failed to build config YAML: %w", err)
	}
	values["__configyaml__"] = configYAML

	rawBpYAML, err := marshalYAMLAllFields(w.blueprint)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal blueprint to YAML: %w", err)
	}
	var bpMap map[string]interface{}
	if err := yaml.Unmarshal(rawBpYAML, &bpMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal blueprint map: %w", err)
	}
	metadata := bpMap["metadata"]
	delete(bpMap, "metadata")
	fileContent := struct {
		Metadata  interface{} `yaml:"metadata"`
		Blueprint interface{} `yaml:"blueprint"`
	}{
		Metadata:  metadata,
		Blueprint: bpMap,
	}
	blueprintYAML, err := marshalYAML2(fileContent)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal blueprint file YAML: %w", err)
	}
	if len(w.blueprintChain) > 0 {
		comment := "# inheritance: " + strings.Join(w.blueprintChain, " -> ") + "\n"
		blueprintYAML = append([]byte(comment), blueprintYAML...)
	}
	values["__blueprintyaml__"] = string(blueprintYAML)

	// Inject git-related env vars from blueprint metadata when the workspace is repo-based.
	if w.blueprint.Metadata.RepoName != "" {
		envMap, _ := values["env"].(map[string]interface{})
		if envMap == nil {
			envMap = make(map[string]interface{})
		}
		envMap["GIT_ADDRESS"] = w.blueprint.Metadata.RepoAddress
		envMap["GIT_REPOOWNER"] = w.blueprint.Metadata.RepoOwner
		envMap["GIT_REPONAME"] = w.blueprint.Metadata.RepoName
		envMap["GIT_REPOREF"] = w.blueprint.Metadata.RepoRef
		values["env"] = envMap
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
		ReleaseName: w.Name,
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

func (w *Workspace) GetPodStatus(ctx context.Context) (*models.WorkspaceStatus, error) {
	pw := NewPodWatcher(w.client.KubeClient(), w.client.TargetNamespace(), w.Name, w.log)
	snap, err := pw.Watch(ctx, nil, false)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("failed to get workspace pod status %s: %w", w.Name, err)
	}
	return snapToWorkspaceStatus(snap), nil
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

// StopPod deletes only the workspace pod, leaving the Helm release and all
// other resources (PVCs, secrets, ConfigMaps) intact.
func (w *Workspace) StopPod(ctx context.Context) error {
	err := w.client.KubeClient().CoreV1().Pods(w.client.TargetNamespace()).Delete(ctx, w.Name, metav1.DeleteOptions{})
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to stop workspace pod %s: %w", w.Name, err)
	}
	return nil
}

func workspaceDetailsFromPod(
	pod *corev1.Pod,
) *models.WorkspaceDetails {
	if pod == nil {
		return nil
	}

	userstrB64, ok := pod.Annotations[helm.AnnotationUserStr]
	if !ok || userstrB64 == "" {
		return nil
	}

	canUser, err := parseCanonicalUserStrFromBase64(userstrB64)
	if err != nil {
		return nil
	}

	canonicalId := pod.Labels[helm.LabelCanonicalId]
	if canonicalId == "" {
		return nil
	}

	appVersion := pod.Labels[helm.LabelAppVersion]
	if appVersion == "" {
		appVersion = "1.0.0"
	}

	var cpu, memory string
	var port int
	for _, c := range pod.Spec.Containers {
		if strings.HasSuffix(c.Name, "k8shell-main") {
			cpu = c.Resources.Limits.Cpu().String()
			memory = c.Resources.Limits.Memory().String()
			for _, p := range c.Ports {
				if p.ContainerPort > 0 {
					port = int(p.ContainerPort)
					break
				}
			}
			break
		}
	}

	if port == 0 {
		port = models.WORKSPACE_PORT
	}

	tlsEnabled := podMountsSecret(pod, canonicalId+"-tls")
	identity := canUser.Identity()

	snap := AnalyzePod(pod, nil, defaultCrashLoopThreshold)
	return &models.WorkspaceDetails{
		WorkspaceStatus: *snapToWorkspaceStatus(&snap),
		Name:            pod.Name,
		Username:        pod.Labels[helm.LabelUsername],
		RepoOwner:       identity.RepoOwner(),
		RepoName:        identity.RepoName(),
		RepoRef:         identity.RepoRef(),
		Blueprint:       pod.Labels[helm.LabelBlueprint],
		Organization:    pod.Labels[helm.LabelOrganization],
		JobId:           pod.Labels[helm.LabelJobId],
		ServerName:      pod.Labels[helm.LabelCanonicalId] + "." + pod.Namespace,
		PodIP:           pod.Status.PodIP,
		Port:            port,
		TLSEnabled:      tlsEnabled,
		AppVersion:      appVersion,
		CPU:             cpu,
		Memory:          memory,
		Hostname:        podHostname(pod),
		Namespace:       pod.Namespace,
	}
}
