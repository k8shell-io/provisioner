package workspace

import (
	"context"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
)

// InjectOptions controls the behaviour of workspace injection into an existing Deployment.
type InjectOptions struct {
	// Namespace is the namespace that contains the target Deployment.
	Namespace string
	// DeploymentName is the name of the existing Deployment to inject into.
	DeploymentName string
	// Timeout is the maximum number of seconds to wait for the injected pods to
	// become Running. 0 means use the default (120 s).
	Timeout int
	// Messages receives streaming status events during injection. May be nil.
	Messages chan models.WorkspaceStreamEvent
}

// EjectOptions controls the behaviour of workspace ejection from a Deployment.
type EjectOptions struct {
	// Namespace is the namespace that contains the target Deployment.
	Namespace string
	// DeploymentName is the name of the Deployment to eject from.
	DeploymentName string
	// Timeout is the maximum number of seconds to wait for the rolling update
	// that removes the injected containers. 0 means use the default (60 s).
	Timeout int
}

// Inject provisions the k8shell workspace by injecting it into an existing
// external Deployment rather than creating a standalone Pod via Helm.
//
// Steps:
//  1. Render the workspace Helm chart (dry-run) to obtain all resources.
//  2. Apply supporting resources (ConfigMaps, PVCs, …) to the target namespace.
//  3. Extract the Pod spec additions (containers, volumes) from the rendered Pod.
//  4. Patch the target Deployment to append those additions (rolling update fires).
//  5. Watch the new pods via PodWatcher until Running (or timeout).
func (w *Workspace) Inject(ctx context.Context, opts *InjectOptions) (*models.WorkspaceStatus, error) {
	if opts == nil {
		return nil, fmt.Errorf("inject options are required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("inject options: namespace is required")
	}
	if opts.DeploymentName == "" {
		return nil, fmt.Errorf("inject options: deploymentName is required")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 120
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("namespace", opts.Namespace).
		Str("deployment", opts.DeploymentName).
		Msg("injecting workspace into deployment")

	// 1. Build Helm values (same as a normal provision).
	values, err := w.Values()
	if err != nil {
		return nil, fmt.Errorf("failed to build helm values: %w", err)
	}
	// Disable the manifest hash check — not applicable for injection mode.
	values["__manifesthash__"] = ""

	// 2. Extract and apply all non-Pod resources to the target namespace.
	resources, err := w.client.WorkspaceResourcesFromTemplate(ctx, values, w.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to render workspace resources: %w", err)
	}

	resourceLabels := map[string]string{
		"k8shell.io/workspace":       w.Name,
		"k8shell.io/inject-target":   opts.DeploymentName,
		"k8shell.io/inject-ns":       opts.Namespace,
		"k8shell.io/managed-by":      "k8shell-provisioner",
		"app.kubernetes.io/instance": w.Name,
	}

	if err := w.client.ApplyNamespacedResources(ctx, opts.Namespace, resources, resourceLabels); err != nil {
		return nil, fmt.Errorf("failed to apply workspace resources to namespace %s: %w", opts.Namespace, err)
	}

	// 3. Build the injection spec (prefixed containers/volumes from the Pod).
	spec, err := w.client.InjectionSpecFromTemplate(ctx, values, w.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to build injection spec: %w", err)
	}

	// 4. Patch the Deployment — fails if already injected.
	startTime := time.Now()
	if err := w.client.InjectIntoDeployment(ctx, opts.Namespace, opts.DeploymentName, w.Name, spec); err != nil {
		return nil, fmt.Errorf("failed to inject into deployment %s/%s: %w", opts.Namespace, opts.DeploymentName, err)
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("deployment", opts.DeploymentName).
		Str("namespace", opts.Namespace).
		Msg("deployment patched, waiting for pods to become running")

	// 5. Watch the pods that belong to the Deployment in the target namespace.
	// PodWatcher identifies pods by the k8shell.io/workspace label that was
	// added to the Deployment's pod template during injection.
	provisionOpts := &ProvisionOptions{
		Timeout:  opts.Timeout,
		Messages: opts.Messages,
	}
	pw := NewPodWatcher(w.client.KubeClient(), opts.Namespace, w.Name, w.log)
	snap, err := pw.WatchByLabel(ctx, "k8shell.io/workspace="+w.Name, provisionOpts)
	if err != nil {
		return nil, fmt.Errorf("error watching injected pods: %w", err)
	}

	status := snapToWorkspaceStatus(snap)
	if status.Status == models.WorkspaceStatusRunning {
		w.log.Info().
			Str("workspace", w.Name).
			Dur("elapsed", time.Since(startTime)).
			Msg("injected workspace is running")
	}
	return status, nil
}

// Eject reverses a previous Inject call:
//  1. Reads injection tracking annotations from the Deployment.
//  2. Patches the Deployment to remove the injected containers/volumes (rolling update fires).
//  3. Deletes all workspace-labeled resources from the target namespace.
func (w *Workspace) Eject(ctx context.Context, opts *EjectOptions) error {
	if opts == nil {
		return fmt.Errorf("eject options are required")
	}
	if opts.Namespace == "" {
		return fmt.Errorf("eject options: namespace is required")
	}
	if opts.DeploymentName == "" {
		return fmt.Errorf("eject options: deploymentName is required")
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("namespace", opts.Namespace).
		Str("deployment", opts.DeploymentName).
		Msg("ejecting workspace from deployment")

	// Step 1+2: read annotations and patch the Deployment.
	if err := w.client.EjectFromDeployment(ctx, opts.Namespace, opts.DeploymentName, w.Name); err != nil {
		return fmt.Errorf("failed to eject from deployment %s/%s: %w", opts.Namespace, opts.DeploymentName, err)
	}

	// Step 3: delete all workspace resources in the target namespace.
	if err := w.client.DeleteNamespacedWorkspaceResources(ctx, opts.Namespace, w.Name); err != nil {
		return fmt.Errorf("failed to delete workspace resources in namespace %s: %w", opts.Namespace, err)
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("namespace", opts.Namespace).
		Str("deployment", opts.DeploymentName).
		Msg("workspace ejected successfully")
	return nil
}

// IsInjected returns true when the named Deployment in the given namespace
// carries the injection annotation for this workspace.
func (w *Workspace) IsInjected(ctx context.Context, namespace, deploymentName string) (bool, error) {
	dep, err := w.client.GetDeployment(ctx, namespace, deploymentName)
	if err != nil {
		return false, err
	}
	return dep.Annotations[helm.AnnotationInjectedWorkspace] == w.Name, nil
}
