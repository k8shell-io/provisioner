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
	"k8s.io/client-go/kubernetes"
)

// InjectOptions controls the behaviour of workspace injection into an existing workload.
type InjectOptions struct {
	// Namespace is the namespace that contains the target workload.
	Namespace string
	// WorkloadName is the name of the existing workload to inject into.
	WorkloadName string
	// WorkloadKind is the kind of the target workload (Deployment, StatefulSet, DaemonSet).
	// Defaults to "Deployment" when empty.
	WorkloadKind string
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

// EjectOptions controls the behaviour of workspace ejection from a workload.
type EjectOptions struct {
	// Namespace is the namespace that contains the target workload.
	Namespace string
	// WorkloadName is the name of the workload to eject from.
	WorkloadName string
	// WorkloadKind is the kind of the target workload (Deployment, StatefulSet, DaemonSet).
	// Defaults to "Deployment" when empty.
	WorkloadKind string
	// Timeout is the maximum number of seconds to wait for the rolling update
	// that removes the injected containers. 0 means use the default (60 s).
	Timeout int
}

// WorkloadOwner identifies the workload that owns a pod injected with a workspace
type WorkloadOwner struct {
	Kind string
	Name string
}

// Inject provisions the k8shell workspace by injecting the workspace containers and volumes into an
// existing workload. The target workload is identified by the namespace and name provided
// in the InjectOptions
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
	if opts.WorkloadName == "" {
		return nil, fmt.Errorf("inject options: workloadName is required")
	}
	if opts.WorkloadKind == "" {
		opts.WorkloadKind = "Deployment"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 120
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("namespace", opts.Namespace).
		Str("kind", opts.WorkloadKind).
		Str("workload", opts.WorkloadName).
		Msg("injecting workspace into workload")

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
		helm.LabelCanonicalId:  opts.WorkspaceCanonicalId,
		helm.LabelInjectTarget: opts.WorkloadKind + "." + opts.WorkloadName,
		helm.LabelManagedBy:    "k8shell-provisioner",
	}

	startTime := time.Now()
	if err := w.client.ApplyNamespacedResources(ctx, opts.Namespace, resources, resourceLabels); err != nil {
		return nil, fmt.Errorf("failed to apply workspace resources to namespace %s: %w", opts.Namespace, err)
	}

	if err := w.ensureSharedStorages(ctx, opts.Namespace, "k8shell-"); err != nil {
		return nil, fmt.Errorf("failed to ensure shared storages: %w", err)
	}

	adapter, err := w.client.GetWorkloadAdapter(ctx, opts.Namespace, opts.WorkloadKind, opts.WorkloadName)
	if err != nil {
		return nil, fmt.Errorf("failed to get workload adapter for %s/%s/%s: %w", opts.WorkloadKind,
			opts.Namespace, opts.WorkloadName, err)
	}
	if err := w.client.InjectIntoWorkload(ctx, adapter, opts.WorkspaceCanonicalId, spec); err != nil {
		return nil, fmt.Errorf("failed to inject into %s %s/%s: %w", opts.WorkloadKind, opts.Namespace,
			opts.WorkloadName, err)
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("kind", opts.WorkloadKind).
		Str("workload", opts.WorkloadName).
		Str("namespace", opts.Namespace).
		Msg("workload patched, waiting for pods to become running")

	provisionOpts := &ProvisionOptions{
		Timeout:  opts.Timeout,
		Messages: opts.Messages,
	}
	pw := NewPodWatcher(w.client.KubeClient(), opts.Namespace, w.Name, w.log)
	snap, err := pw.WatchByLabel(ctx, helm.LabelCanonicalId+"="+opts.WorkspaceCanonicalId, provisionOpts)
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
//  1. Reads injection tracking annotations from the workload.
//  2. Updates the workload to remove the injected containers/volumes (rolling update fires).
//  3. Deletes all workspace-labeled resources from the target namespace.
func (w *Workspace) Eject(ctx context.Context, opts *EjectOptions) error {
	if opts == nil {
		return fmt.Errorf("eject options are required")
	}
	if opts.Namespace == "" {
		return fmt.Errorf("eject options: namespace is required")
	}
	if opts.WorkloadName == "" {
		return fmt.Errorf("eject options: workloadName is required")
	}
	if opts.WorkloadKind == "" {
		opts.WorkloadKind = "Deployment"
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("namespace", opts.Namespace).
		Str("kind", opts.WorkloadKind).
		Str("workload", opts.WorkloadName).
		Msg("ejecting workspace from workload")

	adapter, err := w.client.GetWorkloadAdapter(ctx, opts.Namespace, opts.WorkloadKind, opts.WorkloadName)
	if err != nil {
		return fmt.Errorf("failed to get workload adapter for %s/%s/%s: %w", opts.WorkloadKind, opts.Namespace,
			opts.WorkloadName, err)
	}

	// Step 1+2: read annotations and update the workload.
	canonicalId, err := w.client.EjectFromWorkload(ctx, adapter, w.Name)
	if err != nil {
		return fmt.Errorf("failed to eject from %s %s/%s: %w", opts.WorkloadKind, opts.Namespace,
			opts.WorkloadName, err)
	}

	// Step 3: delete all workspace resources in the target namespace.
	if err := w.client.DeleteNamespacedWorkspaceResources(ctx, opts.Namespace, canonicalId); err != nil {
		return fmt.Errorf("failed to delete workspace resources in namespace %s: %w", opts.Namespace, err)
	}

	w.log.Info().
		Str("workspace", w.Name).
		Str("namespace", opts.Namespace).
		Str("kind", opts.WorkloadKind).
		Str("workload", opts.WorkloadName).
		Msg("workspace ejected successfully")
	return nil
}

// IsInjected returns true when the named workload in the given namespace
// carries the injection annotation for this workspace.
func (w *Workspace) IsInjected(ctx context.Context, namespace, workloadKind, workloadName string) (bool, error) {
	adapter, err := w.client.GetWorkloadAdapter(ctx, namespace, workloadKind, workloadName)
	if err != nil {
		return false, err
	}
	return helm.InjectedCanonicalId(adapter.GetAnnotations()) == w.Name, nil
}

// sanitizeBlueprintForInjection returns a deep copy of bp with branches that
// are not safe to inject into an existing Deployment disabled.
func sanitizeBlueprintForInjection(bp *models.Blueprint) (*models.Blueprint, error) {
	if bp == nil {
		return nil, fmt.Errorf("blueprint is nil")
	}

	raw, err := yaml.Marshal(bp)
	if err != nil {
		return nil, fmt.Errorf("marshal blueprint: %w", err)
	}
	var copy models.Blueprint
	if err := yaml.Unmarshal(raw, &copy); err != nil {
		return nil, fmt.Errorf("unmarshal blueprint copy: %w", err)
	}
	copy.Metadata = bp.Metadata

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

	copy.Podman.Enabled = false
	for name, s := range copy.Podman.Storages {
		s.Enabled = false
		copy.Podman.Storages[name] = s
	}

	copy.Network.NetworkPolicyClass = ""

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

// FindOwnerWorkload identifies the supported workload (Deployment, StatefulSet,
// DaemonSet) that owns the given pod. It returns an error if the pod is missing
// the identifying labels or if the
func FindOwnerWorkload(ctx context.Context, kubeClient kubernetes.Interface, pod *corev1.Pod) (*WorkloadOwner, error) {
	// Fast path: labels stamped during injection.
	if kind, ok := pod.Labels[helm.LabelWorkloadKind]; ok && kind != "" {
		if name, ok := pod.Labels[helm.LabelWorkloadName]; ok && name != "" {
			return &WorkloadOwner{Kind: kind, Name: name}, nil
		}
	}
	return nil, fmt.Errorf("pod %s/%s is missing workload owner labels", pod.Namespace, pod.Name)
}
