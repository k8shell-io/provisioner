package workspace

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// InjectOptions controls the behaviour of workspace injection into an existing Deployment.
type InjectOptions struct {
	// Namespace is the namespace that contains the target Deployment.
	Namespace string
	// DeploymentName is the name of the existing Deployment to inject into.
	DeploymentName string
	// WorkspaceCanonicalId is the canonical identifier of the workspace,
	// used for tracking and labeling injected resources.
	WorkspaceCanonicalId string
	// JobId is the identifier of the provisioning job, used for labeling injected resources.
	JobId string
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

// Inject provisions the k8shell workspace by injecting the workspace containers and volumes into an
// existing Deployment. The target Deployment is identified by the namespace and name provided in the InjectOptions
func (w *Workspace) Inject(ctx context.Context, opts *InjectOptions) (*models.WorkspaceStatus, error) {
	if opts == nil {
		return nil, fmt.Errorf("inject options are required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("inject options: namespace is required")
	}
	if w.config == nil {
		return nil, fmt.Errorf("inject options: workspace config is required")
	}
	if !w.config.AllowsInjectionNamespace(opts.Namespace) {
		return nil, fmt.Errorf("inject options: namespace %q is not allowed by config injectNamespaces", opts.Namespace)
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

	var err error
	w.blueprint, err = sanitizeBlueprintForInjection(w.blueprint)
	if err != nil {
		return nil, fmt.Errorf("failed to sanitize blueprint for injection: %w", err)
	}

	values, err := w.Values()
	if err != nil {
		return nil, fmt.Errorf("failed to build helm values: %w", err)
	}

	resources, err := w.client.WorkspaceResourcesFromTemplate(ctx, values, opts.WorkspaceCanonicalId)
	if err != nil {
		return nil, fmt.Errorf("failed to render workspace resources: %w", err)
	}

	spec, err := w.client.InjectionSpecFromTemplate(ctx, values, opts.WorkspaceCanonicalId, opts.JobId, "k8shell-")
	if err != nil {
		return nil, fmt.Errorf("failed to build injection spec: %w", err)
	}

	resourceLabels := map[string]string{
		"k8shell.io/canonical-id":  opts.WorkspaceCanonicalId,
		"k8shell.io/inject-target": opts.DeploymentName,
		"k8shell.io/managed-by":    "k8shell-provisioner",
	}

	startTime := time.Now()
	if err := w.client.ApplyNamespacedResources(ctx, opts.Namespace, resources, resourceLabels); err != nil {
		return nil, fmt.Errorf("failed to apply workspace resources to namespace %s: %w", opts.Namespace, err)
	}

	if err := w.ensureSharedStorages(ctx, opts.Namespace, "k8shell-"); err != nil {
		return nil, fmt.Errorf("failed to ensure shared storages: %w", err)
	}

	if err := w.client.InjectIntoDeployment(ctx, opts.Namespace, opts.DeploymentName, w.Name, opts.WorkspaceCanonicalId, spec); err != nil {
		return nil, fmt.Errorf("failed to inject into deployment %s/%s: %w", opts.Namespace, opts.DeploymentName, err)
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("deployment", opts.DeploymentName).
		Str("namespace", opts.Namespace).
		Msg("deployment patched, waiting for pods to become running")

	// Watch the pods that belong to the Deployment in the target namespace.
	// PodWatcher identifies pods by the k8shell.io/canonical-id label that was
	// added to the Deployment's pod template during injection.
	provisionOpts := &ProvisionOptions{
		Timeout:  opts.Timeout,
		Messages: opts.Messages,
	}
	pw := NewPodWatcher(w.client.KubeClient(), opts.Namespace, w.Name, w.log)
	snap, err := pw.WatchByLabel(ctx, "k8shell.io/canonical-id="+opts.WorkspaceCanonicalId, provisionOpts)
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
	canonicalId, err := w.client.EjectFromDeployment(ctx, opts.Namespace, opts.DeploymentName, w.Name)
	if err != nil {
		return fmt.Errorf("failed to eject from deployment %s/%s: %w", opts.Namespace, opts.DeploymentName, err)
	}

	// Step 3: delete all workspace resources in the target namespace.
	if err := w.client.DeleteNamespacedWorkspaceResources(ctx, opts.Namespace, canonicalId); err != nil {
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

// sanitizeBlueprintForInjection returns a deep copy of bp with branches that
// are not safe to inject into an existing Deployment disabled.
func sanitizeBlueprintForInjection(bp *models.Blueprint) (*models.Blueprint, error) {
	if bp == nil {
		return nil, fmt.Errorf("blueprint is nil")
	}

	// Deep copy via YAML round-trip so we don't mutate the original.
	raw, err := yaml.Marshal(bp)
	if err != nil {
		return nil, fmt.Errorf("marshal blueprint: %w", err)
	}
	var copy models.Blueprint
	if err := yaml.Unmarshal(raw, &copy); err != nil {
		return nil, fmt.Errorf("unmarshal blueprint copy: %w", err)
	}
	copy.Metadata = bp.Metadata

	// Disable local storages that are incompatible with injection.
	for name, s := range copy.Storages {
		if !s.Enabled {
			continue
		}
		storageType := s.Type
		if storageType == "" {
			storageType = "local"
		}
		if storageType != "local" {
			continue
		}
		if storageClaimUsesReadWriteOnce(s) || strings.HasPrefix(s.Path, "/home/") {
			s.Enabled = false
			copy.Storages[name] = s
		}
	}

	// Disable podman and all podman-defined storages.
	copy.Podman.Enabled = false
	for name, s := range copy.Podman.Storages {
		s.Enabled = false
		copy.Podman.Storages[name] = s
	}

	return &copy, nil
}

// storageClaimUsesReadWriteOnce reports whether the storage's ClaimSpec declares
// a ReadWriteOnce access mode.
func storageClaimUsesReadWriteOnce(s models.Storage) bool {
	accessModes, ok := s.ClaimSpec["accessModes"]
	if !ok {
		return false
	}
	for _, mode := range toStringSlice(accessModes) {
		if strings.EqualFold(strings.TrimSpace(mode), "ReadWriteOnce") {
			return true
		}
	}
	return false
}

func toStringSlice(value interface{}) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// FindOwnerDeploymentName traverses a pod's owner references (pod → ReplicaSet → Deployment)
// and returns the owning Deployment name, or "" if the pod is not owned by a Deployment.
func FindOwnerDeploymentName(ctx context.Context, kubeClient kubernetes.Interface, pod *corev1.Pod) (string, error) {
	for _, owner := range pod.OwnerReferences {
		switch owner.Kind {
		case "Deployment":
			return owner.Name, nil
		case "ReplicaSet":
			rs, err := kubeClient.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			for _, rsOwner := range rs.OwnerReferences {
				if rsOwner.Kind == "Deployment" {
					return rsOwner.Name, nil
				}
			}
		}
	}
	return "", nil
}
