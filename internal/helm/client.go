// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package helm wraps the Helm SDK and the Kubernetes client to manage
// workspace Helm releases and to inject workspace containers into existing
// workloads (Deployments, StatefulSets, DaemonSets). It also owns the label
// and annotation constants shared across the provisioner.
package helm

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	stderrs "errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/rs/zerolog"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	WORKSPACE_CHART_NAME = "k8shell-workspace"
)

// Client is the provisioner's Helm and Kubernetes client. It holds the
// in-memory Helm charts, Kubernetes typed and dynamic clients, and registry
// configuration needed to install, upgrade, and uninstall workspace releases.
type Client struct {
	settings        *cli.EnvSettings
	log             *zerolog.Logger
	kubeClient      kubernetes.Interface
	dynamicClient   dynamic.Interface
	targetNamespace string
	charts          map[string]*chart.Chart
	Registry        config.DefaultRegistry
	PrivateRegistry config.PrivateRegistry
	AppVersion      string
	Commit          string

	// releaseCache and podCache back ListDeployedReleasesCached and
	// ListPodsCached, used only by the read-only workspace lookup RPCs (see
	// internal/server/find.go). Mutating flows must not read through them.
	releaseCache *ttlCache[[]*release.Release]
	podCache     *ttlCache[[]corev1.Pod]
}

// InstallOptions carries parameters shared by Install, Upgrade, and Template
// operations so callers do not need to configure the action objects directly.
type InstallOptions struct {
	ReleaseName     string
	CreateNamespace bool
	ChartName       string
	Values          map[string]interface{}
	Wait            bool
	Labels          map[string]string
	Timeout         int
	AppVersion      string
}

// NewClient constructs a Client using in-cluster Kubernetes credentials, loads
// the embedded workspace Helm chart into memory, and returns a ready-to-use client.
// kubeClientQPS and kubeClientBurst override client-go's default rate limit
// (5 QPS / 10 burst), which otherwise throttles this process client-side
// under concurrent request load; a value <= 0 leaves client-go's default in
// place.
func NewClient(targetNamespace string, registry config.DefaultRegistry, privateRegistry config.PrivateRegistry,
	kubeClientQPS float32, kubeClientBurst int) (*Client, error) {
	settings := cli.New()

	var config *rest.Config
	var err error

	config, err = rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes config: %w", err)
	}
	if kubeClientQPS > 0 {
		config.QPS = kubeClientQPS
	}
	if kubeClientBurst > 0 {
		config.Burst = kubeClientBurst
	}
	log.NewLogger("helm").Info().
		Float32("qps", config.QPS).
		Int("burst", config.Burst).
		Msg("Kubernetes client rate limit configured")

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic Kubernetes client: %w", err)
	}

	charts := make(map[string]*chart.Chart)
	charts[WORKSPACE_CHART_NAME], err = LoadChartFromMemory(WORKSPACE_CHART_NAME)
	if err != nil {
		return nil, fmt.Errorf("failed to load workspace chart: %w", err)
	}

	return &Client{
		log:             log.NewLogger("helm"),
		settings:        settings,
		kubeClient:      kubeClient,
		dynamicClient:   dynamicClient,
		charts:          charts,
		targetNamespace: targetNamespace,
		Registry:        registry,
		PrivateRegistry: privateRegistry,
		releaseCache:    newTTLCache[[]*release.Release](),
		podCache:        newTTLCache[[]corev1.Pod](),
	}, nil
}

// RegistryValues returns the merged registry values for Helm chart templates.
// host comes from DefaultRegistry and is used to prefix image names.
func (c *Client) RegistryValues() map[string]interface{} {
	values := c.Registry.ToValues()
	if c.PrivateRegistry.Host == "" {
		return values
	}
	privateValues := c.PrivateRegistry.ToValues()
	for _, k := range []string{"certCA", "dockerConfigJson", "regcred"} {
		if v, ok := privateValues[k]; ok {
			values[k] = v
		}
	}
	return values
}

// KubeClient returns the underlying Kubernetes typed client.
func (c *Client) KubeClient() kubernetes.Interface {
	return c.kubeClient
}

// DynamicClient returns the underlying dynamic Kubernetes client, used for
// custom resources such as CiliumNetworkPolicy that have no typed client.
func (c *Client) DynamicClient() dynamic.Interface {
	return c.dynamicClient
}

// TargetNamespace returns the target namespace for Helm operations
func (c *Client) TargetNamespace() string {
	return c.targetNamespace
}

// EnsureBase ensures the base namespace resources are present and up to date.
// Resources are applied directly via the Kubernetes API rather than through Helm
func (c *Client) EnsureBase(ctx context.Context) error {
	if c.targetNamespace == "" {
		return fmt.Errorf("target namespace is not set")
	}

	labels := map[string]string{
		"app.kubernetes.io/version":     c.AppVersion,
		"app.kubernetes.io/managed-by":  "k8shell-provisioner",
		"io.k8shell.provisioner/commit": c.Commit,
	}

	privateValues := c.PrivateRegistry.ToValues()

	if dockerConfigJson, ok := privateValues["dockerConfigJson"].(string); ok && dockerConfigJson != "" {
		regcredName, _ := privateValues["regcred"].(string)
		if regcredName == "" {
			regcredName = "regcred"
		}
		if err := c.applySecret(ctx, regcredName, labels, corev1.SecretTypeDockerConfigJson, map[string][]byte{
			".dockerconfigjson": []byte(dockerConfigJson),
		}); err != nil {
			return fmt.Errorf("failed to apply %s secret: %w", regcredName, err)
		}
	}

	if certCA, ok := privateValues["certCA"].(string); ok && certCA != "" {
		if err := c.applyConfigMap(ctx, "registryca", labels, map[string]string{
			"registry-ca.pem": certCA,
		}); err != nil {
			return fmt.Errorf("failed to apply registryca configmap: %w", err)
		}
	}

	return nil
}

// applyConfigMap creates or updates a ConfigMap in the target namespace.
func (c *Client) applyConfigMap(ctx context.Context, name string, labels map[string]string, data map[string]string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.targetNamespace,
			Labels:    labels,
		},
		Data: data,
	}

	existing, err := c.kubeClient.CoreV1().ConfigMaps(c.targetNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get configmap %s: %w", name, err)
		}
		if _, err = c.kubeClient.CoreV1().ConfigMaps(c.targetNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create configmap %s: %w", name, err)
		}
		c.log.Info().Msgf("Created configmap %s in namespace %s", name, c.targetNamespace)
		return nil
	}

	cm.ResourceVersion = existing.ResourceVersion
	if _, err = c.kubeClient.CoreV1().ConfigMaps(c.targetNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update configmap %s: %w", name, err)
	}
	c.log.Info().Msgf("Updated configmap %s in namespace %s", name, c.targetNamespace)
	return nil
}

// applySecret creates or updates a Secret in the target namespace.
func (c *Client) applySecret(ctx context.Context, name string, labels map[string]string, secretType corev1.SecretType, data map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.targetNamespace,
			Labels:    labels,
		},
		Type: secretType,
		Data: data,
	}

	existing, err := c.kubeClient.CoreV1().Secrets(c.targetNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get secret %s: %w", name, err)
		}
		if _, err = c.kubeClient.CoreV1().Secrets(c.targetNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create secret %s: %w", name, err)
		}
		c.log.Info().Msgf("Created secret %s in namespace %s", name, c.targetNamespace)
		return nil
	}

	secret.ResourceVersion = existing.ResourceVersion
	if _, err = c.kubeClient.CoreV1().Secrets(c.targetNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update secret %s: %w", name, err)
	}
	c.log.Info().Msgf("Updated secret %s in namespace %s", name, c.targetNamespace)
	return nil
}

// Template renders the named Helm chart with opts.Values in dry-run / client-only
// mode and returns the resulting YAML manifest string without contacting the cluster.
func (c *Client) Template(ctx context.Context, chartName string, opts InstallOptions) (string, error) {
	actionConfig, err := c.createActionConfig(c.targetNamespace)
	if err != nil {
		return "", err
	}

	install := action.NewInstall(actionConfig)
	install.ReleaseName = opts.ReleaseName
	install.Namespace = c.targetNamespace
	install.DryRun = true
	install.ClientOnly = true
	install.IncludeCRDs = true

	if opts.Timeout > 0 {
		install.Timeout = time.Duration(opts.Timeout) * time.Second
	}

	chart, ok := c.charts[chartName]
	if !ok {
		return "", fmt.Errorf("chart %s not found", chartName)
	}

	chart = c.cloneChart(chart)
	chart.Metadata.Version = c.AppVersion + "-" + c.Commit
	if opts.AppVersion != "" {
		chart.Metadata.AppVersion = opts.AppVersion
	}

	release, err := install.RunWithContext(ctx, chart, opts.Values)
	if err != nil {
		return "", fmt.Errorf("failed to render chart: %w", err)
	}

	return release.Manifest, nil
}

// Install installs a Helm chart in the specified namespace
func (c *Client) Install(ctx context.Context, chartName string, opts InstallOptions) error {
	actionConfig, err := c.createActionConfig(c.targetNamespace)
	if err != nil {
		return err
	}

	install := action.NewInstall(actionConfig)
	install.ReleaseName = opts.ReleaseName
	install.Namespace = c.targetNamespace
	install.CreateNamespace = opts.CreateNamespace
	install.Wait = opts.Wait
	install.Labels = opts.Labels

	if opts.Timeout > 0 {
		install.Timeout = time.Duration(opts.Timeout) * time.Second
	}

	originalChart, ok := c.charts[chartName]
	if !ok {
		return fmt.Errorf("chart %s not found", chartName)
	}

	// Clone the chart to avoid modifying the shared instance
	chart := c.cloneChart(originalChart)

	// Now safely modify the AppVersion
	if opts.AppVersion != "" {
		chart.Metadata.AppVersion = opts.AppVersion
	}
	chart.Metadata.Version = c.AppVersion + "-" + c.Commit

	_, err = install.RunWithContext(ctx, chart, opts.Values)
	if err != nil {
		return fmt.Errorf("failed to install chart: %w", err)
	}

	return nil
}

// List lists Helm releases in a specific namespace
func (c *Client) List(namespace string) ([]*release.Release, error) {
	actionConfig, err := c.createActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	list := action.NewList(actionConfig)
	releases, err := list.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to list releases in namespace %s: %w", namespace, err)
	}
	return releases, nil
}

// ListReleasesBySelector lists Helm releases in namespace whose Secret
// matches selector (a standard Kubernetes label selector, e.g. as built by
// the workspace package's getSelector), narrowed server-side by the
// Kubernetes API — unlike ListWithSelector, which fetches and decodes every
// release in the namespace regardless of selector. This works because Helm
// writes a release's own Labels onto its storage Secret's metadata.labels
// alongside its "owner"/"status"/"name" system labels (see
// helm.sh/helm/v3/pkg/storage/driver/secrets.go's newSecretsObject), so the
// same labels callers already filter pods on are equally queryable on the
// release secret itself. Only "deployed" (i.e. current) releases are
// returned, matching ListWithSelector's ListDeployed state mask.
//
// This also sidesteps createActionConfig/action.NewList, which rebuilds a
// full Kubernetes client (including a discovery client) on every call —
// itself a bigger cost than decoding a handful of releases. This reuses
// c.kubeClient, already built once in NewClient.
func (c *Client) ListReleasesBySelector(ctx context.Context, namespace, selector string) ([]*release.Release, error) {
	secrets, err := c.kubeClient.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: releaseSecretSelector(selector),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list release secrets in namespace %s with selector %s: %w", namespace, selector, err)
	}
	return decodeReleaseSecrets(c.log, secrets.Items), nil
}

// releaseSecretSelector ANDs the Helm release system labels ("owner=helm",
// "status=deployed") that scope a plain Secrets List to Helm's current
// releases onto selector, the caller's own label selector (empty is valid —
// it means "every deployed release").
func releaseSecretSelector(selector string) string {
	base := "owner=helm,status=deployed"
	if selector == "" {
		return base
	}
	return base + "," + selector
}

// decodeReleaseSecrets decodes each release Secret's "release" data field,
// skipping (and logging) any that fail to decode rather than failing the
// whole call — a single malformed secret shouldn't take down every listing.
func decodeReleaseSecrets(log *zerolog.Logger, secrets []corev1.Secret) []*release.Release {
	releases := make([]*release.Release, 0, len(secrets))
	for i := range secrets {
		s := &secrets[i]
		rel, err := decodeHelmReleaseSecret(s.Data["release"])
		if err != nil {
			log.Warn().Err(err).Str("secret", s.Name).Msg("failed to decode Helm release secret")
			continue
		}
		rel.Labels = s.Labels
		releases = append(releases, rel)
	}
	return releases
}

// decodeHelmReleaseSecret decodes a Helm release Secret's "release" data
// field. It mirrors the unexported decodeRelease in
// helm.sh/helm/v3/pkg/storage/driver/util.go, which Helm has no public
// equivalent for: base64-decode, then gunzip if the gzip magic header is
// present (releases stored before Helm introduced compression are left
// as-is), then JSON-unmarshal into a release.Release.
func decodeHelmReleaseSecret(data []byte) (*release.Release, error) {
	b, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}

	if len(b) > 3 && bytes.Equal(b[0:3], []byte{0x1f, 0x8b, 0x08}) {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer r.Close()
		if b, err = io.ReadAll(r); err != nil {
			return nil, fmt.Errorf("gzip read: %w", err)
		}
	}

	var rel release.Release
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}
	return &rel, nil
}

// ListWithSelector lists Helm releases in namespace, applying selector.
// Despite taking a selector, this is not server-side filtered: Helm's List
// action decodes every release secret in the namespace regardless of
// selector, then filters in memory (see helm.sh/helm/v3/pkg/storage/driver
// Secrets.List). Prefer ListReleasesBySelector, which does filter
// server-side, or ListDeployedReleasesCached when the caller can tolerate a
// short-lived cache and apply its own filtering in memory.
func (c *Client) ListWithSelector(namespace, selector string) ([]*release.Release, error) {
	actionConfig, err := c.createActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	list := action.NewList(actionConfig)

	if selector != "" {
		list.Selector = selector
	}

	list.StateMask = action.ListDeployed
	list.SortReverse = true
	list.ByDate = true

	releases, err := list.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to list releases in namespace %s with selector %s: %w", namespace, selector, err)
	}
	return releases, nil
}

// ListDeployedReleasesCached returns every deployed Helm release in
// namespace, served from a short-lived cache shared across concurrent
// callers (see ttlCache). Since a plain Secrets List returns every release
// regardless of the caller's own filter anyway, callers needing a subset
// should filter this unfiltered result themselves — e.g. with
// k8s.io/apimachinery/pkg/labels.Selector.Matches against each release's
// Labels — rather than passing a selector, since that's what actually
// benefits from the cache when concurrent requests differ only by, say, a
// workspace name.
//
// Only for read-only lookups: the cache can be up to listCacheTTL stale, so
// callers that must observe a just-completed mutation (stop, delete, eject)
// must use List or ListReleasesBySelector instead.
func (c *Client) ListDeployedReleasesCached(ctx context.Context, namespace string) ([]*release.Release, error) {
	return c.releaseCache.getOrFetch(namespace, func() ([]*release.Release, error) {
		secrets, err := c.kubeClient.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: releaseSecretSelector(""),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list release secrets in namespace %s: %w", namespace, err)
		}
		return decodeReleaseSecrets(c.log, secrets.Items), nil
	})
}

// ListPodsCached lists pods in namespace matching selector, served from a
// short-lived cache shared across concurrent callers with an identical
// (namespace, selector) pair (see ttlCache) — which read-only workspace
// lookups very often share, since request-specific narrowing (e.g. a
// workspace name) is usually applied by the caller after listing rather than
// folded into the selector.
//
// Only for read-only lookups: the cache can be up to listCacheTTL stale, so
// callers that must observe a just-completed mutation (stop, delete, eject)
// must list pods directly via KubeClient() instead.
func (c *Client) ListPodsCached(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	return c.podCache.getOrFetch(namespace+"|"+selector, func() ([]corev1.Pod, error) {
		list, err := c.kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, err
		}
		return list.Items, nil
	})
}

// ListAllNamespaces lists Helm releases across all namespaces
func (c *Client) ListAllNamespaces() ([]*release.Release, error) {
	actionConfig, err := c.createActionConfig("") // Empty namespace means all namespaces
	if err != nil {
		return nil, err
	}

	list := action.NewList(actionConfig)
	list.AllNamespaces = true // This enables listing across all namespaces
	releases, err := list.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to list releases across all namespaces: %w", err)
	}
	return releases, nil
}

// PodFromRelease extracts the Pod object from the stored Helm release manifest.
// This allows re-creating only the pod without a full Helm upgrade cycle.
func (c *Client) PodFromRelease(releaseName string) (*corev1.Pod, error) {
	rel, err := c.GetRelease(releaseName)
	if err != nil {
		return nil, err
	}
	return PodManifestFromRelease(rel)
}

// PodManifestFromRelease extracts the Pod object from an already-fetched Helm
// release's stored manifest (e.g. from List/ListWithSelector), without an
// extra round-trip to Helm's release storage. Used to recover a workspace's
// labels, annotations and spec after its live pod has been deleted (StopPod)
// while the Helm release itself is left installed.
func PodManifestFromRelease(rel *release.Release) (*corev1.Pod, error) {
	for _, doc := range strings.Split(rel.Manifest, "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" || !strings.Contains(doc, "kind: Pod") {
			continue
		}
		var pod corev1.Pod
		if err := sigsyaml.Unmarshal([]byte(doc), &pod); err != nil {
			return nil, fmt.Errorf("failed to decode pod manifest from release %s: %w", rel.Name, err)
		}
		if pod.Kind == "Pod" {
			pod.ResourceVersion = ""
			pod.UID = ""
			pod.CreationTimestamp = metav1.Time{}
			pod.Status = corev1.PodStatus{}
			return &pod, nil
		}
	}
	return nil, fmt.Errorf("pod not found in release %s manifest", rel.Name)
}

// UpdatePATSecret refreshes the token key of a workspace's PAT secret
// (named "<releaseName>-pat", rendered by the workspace chart) in place,
// preserving its existing labels so it stays tracked by the owning Helm
// release. This lets a rotated PAT be picked up by a pod that is recreated
// directly from a stored release manifest (see Workspace.doStart), without
// going through a full Helm install/upgrade.
func (c *Client) UpdatePATSecret(ctx context.Context, releaseName, token string) error {
	name := releaseName + "-pat"

	existing, err := c.kubeClient.CoreV1().Secrets(c.targetNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get PAT secret %s: %w", name, err)
	}

	existing.Data = map[string][]byte{"token": []byte(token)}
	existing.StringData = nil
	if _, err := c.kubeClient.CoreV1().Secrets(c.targetNamespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update PAT secret %s: %w", name, err)
	}
	return nil
}

// GetRelease gets information about a specific release in a namespace
func (c *Client) GetRelease(releaseName string) (*release.Release, error) {
	actionConfig, err := c.createActionConfig(c.targetNamespace)
	if err != nil {
		return nil, err
	}

	get := action.NewGet(actionConfig)
	release, err := get.Run(releaseName)
	if err != nil {
		return nil, fmt.Errorf("failed to get release %s in namespace %s: %w", releaseName, c.targetNamespace, err)
	}
	return release, nil
}

// Uninstall removes a Helm release from a specific namespace
func (c *Client) Uninstall(releaseName string, timeout int, wait bool) error {
	actionConfig, err := c.createActionConfig(c.targetNamespace)
	if err != nil {
		return err
	}

	uninstall := action.NewUninstall(actionConfig)
	uninstall.Wait = wait
	uninstall.Timeout = time.Duration(timeout) * time.Second
	_, err = uninstall.Run(releaseName)
	if err != nil {
		return fmt.Errorf("failed to uninstall release %s from namespace %s: %w", releaseName, c.targetNamespace, err)
	}
	return nil
}

// CanUpgrade performs a client-side dry-run upgrade to check whether the given
// release can be upgraded. It fails if the release does not exist or is in a
// pending state. Use this before an actual Upgrade to surface config errors early.
func (c *Client) CanUpgrade(ctx context.Context, opts InstallOptions) error {
	actionConfig, err := c.createActionConfig(c.targetNamespace)
	if err != nil {
		return err
	}

	// release must exist and not be pending
	get := action.NewGet(actionConfig)
	existing, err := get.Run(opts.ReleaseName)
	if err != nil {
		if stderrs.Is(err, driver.ErrReleaseNotFound) {
			return fmt.Errorf("release %q not found in namespace %q", opts.ReleaseName, c.targetNamespace)
		}
		return fmt.Errorf("failed to get release %q: %w", opts.ReleaseName, err)
	}
	if existing != nil && existing.Info != nil {
		switch existing.Info.Status {
		case release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback:
			return fmt.Errorf("release %q is in a pending state (%s); cannot upgrade now", opts.ReleaseName, existing.Info.Status)
		}
	}

	upgrade := action.NewUpgrade(actionConfig)
	upgrade.Namespace = c.targetNamespace
	upgrade.Wait = false
	upgrade.DryRun = true
	upgrade.DisableHooks = true
	upgrade.Labels = opts.Labels

	if opts.Timeout > 0 {
		upgrade.Timeout = time.Duration(opts.Timeout) * time.Second
	}

	originalChart, ok := c.charts[opts.ChartName]
	if !ok {
		return fmt.Errorf("chart %s not found", opts.ChartName)
	}

	chart := c.cloneChart(originalChart)
	if opts.AppVersion != "" {
		chart.Metadata.AppVersion = opts.AppVersion
	}
	chart.Metadata.Version = c.AppVersion + "-" + c.Commit

	_, err = upgrade.RunWithContext(ctx, opts.ReleaseName, chart, opts.Values)
	if err != nil {
		return fmt.Errorf("the release %s cannot be upgraded: %w", opts.ReleaseName, err)
	}

	return nil
}

// CanUpgradeDryRunServer performs a server-side dry-run upgrade, which lets
// the Kubernetes API server validate admission webhooks and resource constraints
// without applying any changes.
func (c *Client) CanUpgradeDryRunServer(ctx context.Context, opts InstallOptions) error {
	actionConfig, err := c.createActionConfig(c.targetNamespace)
	if err != nil {
		return err
	}

	upgrade := action.NewUpgrade(actionConfig)
	upgrade.Namespace = c.targetNamespace
	upgrade.DryRun = true
	upgrade.DryRunOption = "server"
	upgrade.DisableHooks = true
	upgrade.Wait = false

	originalChart, ok := c.charts[opts.ChartName]
	if !ok {
		return fmt.Errorf("chart %s not found", opts.ChartName)
	}
	ch := c.cloneChart(originalChart)

	_, err = upgrade.RunWithContext(ctx, opts.ReleaseName, ch, opts.Values)
	return err
}

// Upgrade upgrades a Helm release in the specified namespace
func (c *Client) Upgrade(ctx context.Context, opts InstallOptions) error {
	actionConfig, err := c.createActionConfig(c.targetNamespace)
	if err != nil {
		return err
	}

	// release must exist and not be pending
	get := action.NewGet(actionConfig)
	existing, err := get.Run(opts.ReleaseName)
	if err != nil {
		if stderrs.Is(err, driver.ErrReleaseNotFound) {
			return fmt.Errorf("release %q not found in namespace %q", opts.ReleaseName, c.targetNamespace)
		}
		return fmt.Errorf("failed to get release %q: %w", opts.ReleaseName, err)
	}
	if existing != nil && existing.Info != nil {
		switch existing.Info.Status {
		case release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback:
			return fmt.Errorf("release %q is in a pending state (%s); cannot upgrade now", opts.ReleaseName, existing.Info.Status)
		}
	}

	upgrade := action.NewUpgrade(actionConfig)
	upgrade.Namespace = c.targetNamespace
	upgrade.Wait = true
	upgrade.Atomic = true
	upgrade.CleanupOnFail = true
	upgrade.DisableHooks = true
	upgrade.Force = false

	if opts.Timeout > 0 {
		upgrade.Timeout = time.Duration(opts.Timeout) * time.Second
	}

	originalChart, ok := c.charts[opts.ChartName]
	if !ok {
		return fmt.Errorf("chart %s not found", opts.ChartName)
	}

	ch := c.cloneChart(originalChart)
	if opts.AppVersion != "" {
		ch.Metadata.AppVersion = opts.AppVersion
	}
	ch.Metadata.Version = c.AppVersion + "-" + c.Commit

	oldD, _ := manifestDigest(existing.Manifest)

	rel, err := upgrade.RunWithContext(ctx, opts.ReleaseName, ch, opts.Values)
	if err != nil {
		return fmt.Errorf("the release %s cannot be upgraded: %w", opts.ReleaseName, err)
	}

	newD, _ := manifestDigest(rel.Manifest)

	c.log.Info().
		Str("release", opts.ReleaseName).
		Str("namespace", c.targetNamespace).
		Int("revision", rel.Version).
		Str("oldManifestDigest", oldD).
		Str("newManifestDigest", newD).
		Msg("helm upgrade completed")

	return nil
}

// UpdateReleaseLabels updates the labels of a Helm release in a specific namespace
func (c *Client) UpdateReleaseLabels(releaseName, namespace string, labels map[string]string) error {
	actionConfig, err := c.createActionConfig(namespace)
	if err != nil {
		return err
	}

	get := action.NewGet(actionConfig)
	release, err := get.Run(releaseName)
	if err != nil {
		return fmt.Errorf("failed to get release %s: %w", releaseName, err)
	}

	if release.Labels == nil {
		release.Labels = make(map[string]string)
	}

	for k, v := range labels {
		release.Labels[k] = v
	}

	upgrade := action.NewUpgrade(actionConfig)
	upgrade.ReuseValues = true
	upgrade.Wait = false
	upgrade.DisableHooks = true

	_, err = upgrade.Run(releaseName, release.Chart, release.Config)
	if err != nil {
		return fmt.Errorf("failed to update release labels: %w", err)
	}

	return nil
}

// createActionConfig creates a new action configuration for a specific namespace
func (c *Client) createActionConfig(namespace string) (*action.Configuration, error) {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(
		c.settings.RESTClientGetter(),
		namespace,
		os.Getenv("HELM_DRIVER"),
		c.log.Debug().Msgf,
	); err != nil {
		return nil, fmt.Errorf("failed to initialize Helm action config for namespace %s: %w", namespace, err)
	}

	return actionConfig, nil
}

// cloneChart creates a deep copy of a chart to avoid modifying the original
func (c *Client) cloneChart(original *chart.Chart) *chart.Chart {
	cloned := &chart.Chart{
		Metadata:  cloneMetadata(original.Metadata),
		Lock:      original.Lock,
		Templates: original.Templates,
		Values:    original.Values,
		Schema:    original.Schema,
		Files:     original.Files,
	}

	for _, dep := range original.Dependencies() {
		cloned.AddDependency(dep)
	}

	return cloned
}

// *** helpers

// cloneMetadata creates a copy of chart metadata
func cloneMetadata(original *chart.Metadata) *chart.Metadata {
	if original == nil {
		return nil
	}

	cloned := &chart.Metadata{
		Name:        original.Name,
		Home:        original.Home,
		Sources:     append([]string(nil), original.Sources...),
		Version:     original.Version,
		Description: original.Description,
		Keywords:    append([]string(nil), original.Keywords...),
		Maintainers: make([]*chart.Maintainer, len(original.Maintainers)),
		Icon:        original.Icon,
		APIVersion:  original.APIVersion,
		Condition:   original.Condition,
		Tags:        original.Tags,
		AppVersion:  original.AppVersion,
		Deprecated:  original.Deprecated,
		Type:        original.Type,
		KubeVersion: original.KubeVersion,
	}

	for i, maintainer := range original.Maintainers {
		cloned.Maintainers[i] = &chart.Maintainer{
			Name:  maintainer.Name,
			Email: maintainer.Email,
			URL:   maintainer.URL,
		}
	}

	if original.Annotations != nil {
		cloned.Annotations = make(map[string]string)
		for k, v := range original.Annotations {
			cloned.Annotations[k] = v
		}
	}

	return cloned
}

// normalizeManifest strips trailing whitespace, blank lines, "---" separators,
// and "# Source:" comments from a Helm manifest so that semantically identical
// manifests that differ only in formatting produce the same digest.
func normalizeManifest(m string) (string, error) {
	m = strings.ReplaceAll(m, "\r\n", "\n")
	m = strings.TrimSpace(m)

	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(m))
	sc.Buffer(make([]byte, 1024), 10*1024*1024)

	for sc.Scan() {
		line := sc.Text()
		line = strings.TrimRight(line, " \t")

		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if trim == "---" {
			continue
		}
		if strings.HasPrefix(line, "# Source:") {
			continue
		}

		b.WriteString(line)
		b.WriteByte('\n')
	}

	if err := sc.Err(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// manifestDigest returns the SHA-256 hex digest of a normalised Helm manifest,
// used to detect whether an upgrade actually changes any rendered resource.
func manifestDigest(m string) (string, error) {
	n, err := normalizeManifest(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:]), nil
}
