package workspace

import (
	"context"
	"fmt"

	identity "github.com/k8shell-io/identity/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/k8shell-io/provisioner/pkg/models"
	"gopkg.in/yaml.v3"
)

// Workspace represents a workspace with Helm client
type Workspace struct {
	client    *helm.Client
	namespace string
	blueprint *models.Blueprint
	user      *identity.User
}

// NewWorkspace creates a new workspace with the specified Helm chart
func NewWorkspace(blueprint *models.Blueprint, user *identity.User,
	client *helm.Client) (*Workspace, error) {
	return &Workspace{
		client:    client,
		blueprint: blueprint,
		user:      user,
		namespace: "k8shell-dev-testing",
	}, nil
}

func (w *Workspace) Name() string {
	return w.blueprint.Name + "-" + w.user.Username
}

func (w *Workspace) Values() (map[string]interface{}, error) {
	values, err := w.blueprint.Values()
	if err != nil {
		return nil, err
	}

	tls, key, err := w.generateSelfSignedCert(w.Name())
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
	values["__namespace__"] = w.namespace
	values["__workspace__"] = w.Name()
	values["__blueprint__"] = w.blueprint.Name
	values["__username__"] = w.user.Username
	values["__organization__"] = "org1"
	values["__tlscrt__"] = tls
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
		Namespace:   w.namespace,
		Values:      values,
		Wait:        false,
		Timeout:     20,
	})
	if err != nil {
		return "", err
	}
	return out, nil
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
