package helm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrs "errors"
	"fmt"
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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	WORKSPACE_CHART_NAME      = "k8shell-workspace"
	BASE_WORKSPACE_CHART_NAME = "base-workspace"
)

type Client struct {
	settings             *cli.EnvSettings
	log                  *zerolog.Logger
	kubeClient           kubernetes.Interface
	targetNamespace      string
	charts               map[string]*chart.Chart
	Registry             config.DefaultRegistry
	JWTVerifierPublicKey string
}

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

func NewClient(targetNamespace string, registry config.DefaultRegistry) (*Client, error) {
	settings := cli.New()

	var config *rest.Config
	var err error

	config, err = rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes config: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	charts := make(map[string]*chart.Chart)
	charts[WORKSPACE_CHART_NAME], err = LoadChartFromMemory(WORKSPACE_CHART_NAME)
	if err != nil {
		return nil, fmt.Errorf("failed to load workspace chart: %w", err)
	}

	charts[BASE_WORKSPACE_CHART_NAME], err = LoadChartFromMemory(BASE_WORKSPACE_CHART_NAME)
	if err != nil {
		return nil, fmt.Errorf("failed to load base workspace chart: %w", err)
	}

	return &Client{
		log:             log.NewLogger("helm"),
		settings:        settings,
		kubeClient:      kubeClient,
		charts:          charts,
		targetNamespace: targetNamespace,
		Registry:        registry,
	}, nil
}

func (c *Client) KubeClient() kubernetes.Interface {
	return c.kubeClient
}

// TargetNamespace returns the target namespace for Helm operations
func (c *Client) TargetNamespace() string {
	return c.targetNamespace
}

// EnsureBase ensures that the target namespace is configured and exists
func (c *Client) EnsureBase(ctx context.Context) error {
	if c.targetNamespace == "" {
		return fmt.Errorf("target namespace is not set")
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       BASE_WORKSPACE_CHART_NAME,
		"app.kubernetes.io/instance":   BASE_WORKSPACE_CHART_NAME,
		"app.kubernetes.io/version":    "1.0.0",
		"app.kubernetes.io/managed-by": "k8shell-provisioner",
		"k8shell.io/app":               BASE_WORKSPACE_CHART_NAME,
	}

	values := map[string]interface{}{
		"__registry__":             c.Registry.ToValues(),
		"__jwtVerifierPublicKey__": c.JWTVerifierPublicKey,
	}

	r, err := c.ListWithSelector(c.targetNamespace, "app.kubernetes.io/name="+BASE_WORKSPACE_CHART_NAME)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to list releases in namespace %s: %w", c.targetNamespace, err)
		}
	}

	if len(r) > 0 {
		return c.Upgrade(ctx, InstallOptions{
			ReleaseName: BASE_WORKSPACE_CHART_NAME,
			ChartName:   BASE_WORKSPACE_CHART_NAME,
			Values:      values,
			Wait:        true,
			Labels:      labels,
		})
	}

	return c.Install(ctx, BASE_WORKSPACE_CHART_NAME, InstallOptions{
		ReleaseName:     BASE_WORKSPACE_CHART_NAME,
		ChartName:       BASE_WORKSPACE_CHART_NAME,
		CreateNamespace: false,
		Values:          values,
		Wait:            true,
		Labels:          labels,
	})
}

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

// ListWithSelector lists Helm releases using native Helm label selector (fastest)
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

	_, err = upgrade.RunWithContext(ctx, opts.ReleaseName, chart, opts.Values)
	if err != nil {
		return fmt.Errorf("the release %s cannot be upgraded: %w", opts.ReleaseName, err)
	}

	return nil
}

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

func manifestDigest(m string) (string, error) {
	n, err := normalizeManifest(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:]), nil
}
