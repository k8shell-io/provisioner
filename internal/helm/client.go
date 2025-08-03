package helm

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const WORKSPACE_CHART_NAME = "k8shell-workspace"

type Client struct {
	settings   *cli.EnvSettings
	kubeClient kubernetes.Interface
	charts     map[string]*chart.Chart
}

type InstallOptions struct {
	ReleaseName string
	Namespace   string
	ChartName   string
	Values      map[string]interface{}
	Wait        bool
	Timeout     int
}

// NewClient creates a new Helm client
func NewClient() (*Client, error) {
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

	return &Client{
		settings:   settings,
		kubeClient: kubeClient,
		charts:     charts,
	}, nil
}

func (c *Client) Template(ctx context.Context, chartName string, opts InstallOptions) (string, error) {
	actionConfig, err := c.createActionConfig(opts.Namespace)
	if err != nil {
		return "", err
	}

	install := action.NewInstall(actionConfig)
	install.ReleaseName = opts.ReleaseName
	install.Namespace = opts.Namespace
	install.CreateNamespace = true
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
	actionConfig, err := c.createActionConfig(opts.Namespace)
	if err != nil {
		return err
	}

	install := action.NewInstall(actionConfig)
	install.ReleaseName = opts.ReleaseName
	install.Namespace = opts.Namespace
	install.CreateNamespace = true
	install.Wait = opts.Wait

	if opts.Timeout > 0 {
		install.Timeout = time.Duration(opts.Timeout) * time.Second
	}

	chart, ok := c.charts[chartName]
	if !ok {
		return fmt.Errorf("chart %s not found", chartName)
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
func (c *Client) GetRelease(releaseName, namespace string) (*release.Release, error) {
	actionConfig, err := c.createActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	get := action.NewGet(actionConfig)
	release, err := get.Run(releaseName)
	if err != nil {
		return nil, fmt.Errorf("failed to get release %s in namespace %s: %w", releaseName, namespace, err)
	}
	return release, nil
}

// Uninstall removes a Helm release from a specific namespace
func (c *Client) Uninstall(releaseName, namespace string) error {
	actionConfig, err := c.createActionConfig(namespace)
	if err != nil {
		return err
	}

	uninstall := action.NewUninstall(actionConfig)
	_, err = uninstall.Run(releaseName)
	if err != nil {
		return fmt.Errorf("failed to uninstall release %s from namespace %s: %w", releaseName, namespace, err)
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
		log.Printf,
	); err != nil {
		return nil, fmt.Errorf("failed to initialize Helm action config for namespace %s: %w", namespace, err)
	}

	return actionConfig, nil
}
