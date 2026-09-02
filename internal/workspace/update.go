// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sigsyaml "sigs.k8s.io/yaml"
)

// mainContainerName is the name of the workspace's primary container in the
// k8shell-workspace chart. Resource resizes target this container.
const mainContainerName = "k8shell-main"

// npClasses is the set of predefined network policy classes the
// k8shell-workspace chart knows how to render.
var npClasses = map[string]bool{
	"workspace":    true,
	"system":       true,
	"isolated":     true,
	"user":         true,
	"organization": true,
}

// ciliumNetworkPolicyGVR is the GroupVersionResource for CiliumNetworkPolicy
// objects, rendered by the chart for the "system" class on Cilium clusters.
var ciliumNetworkPolicyGVR = schema.GroupVersionResource{
	Group:    "cilium.io",
	Version:  "v2",
	Resource: "ciliumnetworkpolicies",
}

// UpdateOptions describes an out-of-band change to a workspace's CPU/memory
// limits and/or its network policy. It is applied directly to the live
// Kubernetes objects, leaving the Helm release untouched.
type UpdateOptions struct {
	// ChangeResources gates the CPU/memory resize. When true, CPU and Memory
	// are applied to the workspace's main container via the pods/resize
	// subresource; an empty CPU or Memory leaves that limit unchanged.
	ChangeResources bool
	CPU             string
	Memory          string

	// ChangeNetwork gates the network policy update. When true, the workspace's
	// NetworkPolicies are re-rendered from the chart and re-applied.
	ChangeNetwork bool
	// NetworkPolicyClass is the desired class. Empty keeps the workspace's
	// current class (from its blueprint values) and only re-applies egress.
	NetworkPolicyClass string
	// ReplaceEgress gates AllowEgressToCIDRs / AllowEgressToPods. When true they
	// replace the workspace's egress shortcuts wholesale (empty lists clear
	// them); when false the blueprint's existing egress rules are kept.
	ReplaceEgress      bool
	AllowEgressToCIDRs []string
	AllowEgressToPods  []map[string]string
}

// UpdateResult reports the settings in effect after UpdateResourcesAndNetwork.
type UpdateResult struct {
	ResourcesChanged          bool
	AppliedCPU                string
	AppliedMemory             string
	NetworkChanged            bool
	AppliedNetworkPolicyClass string
}

// UpdateResourcesAndNetwork applies opts to the workspace's live Kubernetes
// objects. Resources are resized first (it needs a running pod); the network
// policy is re-applied second. Either step is skipped when its Change* gate is
// false. The Helm release is never modified, so the workspace reverts to its
// blueprint's resources and network class the next time the release is
// re-applied.
func (w *Workspace) UpdateResourcesAndNetwork(ctx context.Context, opts UpdateOptions) (*UpdateResult, error) {
	res := &UpdateResult{}

	if opts.ChangeResources {
		cpu, mem, err := w.resizeMainContainer(ctx, opts.CPU, opts.Memory)
		if err != nil {
			return nil, err
		}
		res.ResourcesChanged = true
		res.AppliedCPU = cpu
		res.AppliedMemory = mem
	}

	if opts.ChangeNetwork {
		class, err := w.reapplyNetworkPolicies(ctx, opts)
		if err != nil {
			return nil, err
		}
		res.NetworkChanged = true
		res.AppliedNetworkPolicyClass = class
	}

	return res, nil
}

// resizeMainContainer changes the CPU and/or memory limits of the workspace's
// main container in place, using the pods/resize subresource. An empty cpu or
// memory keeps the current value for that resource. It returns the limits in
// effect after the patch.
func (w *Workspace) resizeMainContainer(ctx context.Context, cpu, memory string) (string, string, error) {
	pods := w.client.KubeClient().CoreV1().Pods(w.client.TargetNamespace())

	pod, err := pods.Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return "", "", fmt.Errorf("%w: %s", models.ErrWorkspaceNotFound, w.Name)
		}
		return "", "", fmt.Errorf("failed to get workspace pod %s: %w", w.Name, err)
	}

	var current *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == mainContainerName {
			current = &pod.Spec.Containers[i]
			break
		}
	}
	if current == nil {
		return "", "", fmt.Errorf("workspace pod %s has no %q container", w.Name, mainContainerName)
	}

	limits := corev1.ResourceList{}
	for k, v := range current.Resources.Limits {
		limits[k] = v
	}

	if cpu != "" {
		q, perr := resource.ParseQuantity(cpu)
		if perr != nil {
			return "", "", fmt.Errorf("%w: invalid cpu quantity %q: %v", models.ErrInvalidParameters, cpu, perr)
		}
		limits[corev1.ResourceCPU] = q
	}
	if memory != "" {
		q, perr := resource.ParseQuantity(memory)
		if perr != nil {
			return "", "", fmt.Errorf("%w: invalid memory quantity %q: %v", models.ErrInvalidParameters, memory, perr)
		}
		limits[corev1.ResourceMemory] = q
	}

	limitsPatch := map[string]interface{}{}
	if q, ok := limits[corev1.ResourceCPU]; ok {
		limitsPatch["cpu"] = q.String()
	}
	if q, ok := limits[corev1.ResourceMemory]; ok {
		limitsPatch["memory"] = q.String()
	}

	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"name":      mainContainerName,
					"resources": map[string]interface{}{"limits": limitsPatch},
				},
			},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to build resize patch: %w", err)
	}

	updated, err := pods.Patch(ctx, w.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "resize")
	if err != nil {
		return "", "", fmt.Errorf("failed to resize workspace pod %s: %w", w.Name, err)
	}

	appliedCPU, appliedMemory := "", ""
	for i := range updated.Spec.Containers {
		if updated.Spec.Containers[i].Name == mainContainerName {
			appliedCPU = updated.Spec.Containers[i].Resources.Limits.Cpu().String()
			appliedMemory = updated.Spec.Containers[i].Resources.Limits.Memory().String()
			break
		}
	}
	w.log.Info().Str("workspace", w.Name).Str("cpu", appliedCPU).Str("memory", appliedMemory).
		Msg("resized workspace main container")
	return appliedCPU, appliedMemory, nil
}

// reapplyNetworkPolicies re-renders the workspace Helm chart with an overridden
// network block and applies the resulting NetworkPolicy (and, on Cilium
// clusters, CiliumNetworkPolicy) objects directly, replacing whatever the
// original release installed. The workspace pod's k8shell.io/network-policy
// label is updated to match. It returns the network policy class now in effect.
func (w *Workspace) reapplyNetworkPolicies(ctx context.Context, opts UpdateOptions) (string, error) {
	if w.blueprint == nil {
		return "", fmt.Errorf("blueprint is nil for workspace %s", w.Name)
	}

	class := opts.NetworkPolicyClass
	if class == "" {
		class = w.blueprint.Network.NetworkPolicyClass
	}
	if class == "" {
		class = "workspace"
	}
	if !npClasses[class] {
		return "", fmt.Errorf("%w: unknown network policy class %q", models.ErrInvalidParameters, class)
	}

	canonicalID := w.canonicalId
	if canonicalID == "" && w.userStr != nil {
		canonicalID = w.userStr.CanonicalId()
	}
	if canonicalID == "" {
		return "", fmt.Errorf("cannot determine canonical id for workspace %s", w.Name)
	}

	// Override the in-memory blueprint network block, then re-render the chart.
	// The Workspace is request-scoped, so this mutation is not observed elsewhere.
	w.blueprint.Network.NetworkPolicyClass = class
	if opts.ReplaceEgress {
		w.blueprint.Network.AllowEgressToCIDRs = opts.AllowEgressToCIDRs
		w.blueprint.Network.AllowEgressToPods = opts.AllowEgressToPods
	}

	manifest, err := w.Template(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to render workspace chart for %s: %w", w.Name, err)
	}

	ns := w.client.TargetNamespace()

	var netPols []*networkingv1.NetworkPolicy
	var ciliumPols []*unstructured.Unstructured
	for _, doc := range strings.Split(manifest, "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := sigsyaml.Unmarshal([]byte(doc), &probe); err != nil {
			continue
		}
		switch probe.Kind {
		case "NetworkPolicy":
			np := &networkingv1.NetworkPolicy{}
			if err := sigsyaml.Unmarshal([]byte(doc), np); err != nil {
				return "", fmt.Errorf("failed to decode rendered NetworkPolicy for %s: %w", w.Name, err)
			}
			np.Namespace = ns
			netPols = append(netPols, np)
		case "CiliumNetworkPolicy":
			u := &unstructured.Unstructured{}
			if err := sigsyaml.Unmarshal([]byte(doc), u); err != nil {
				return "", fmt.Errorf("failed to decode rendered CiliumNetworkPolicy for %s: %w", w.Name, err)
			}
			u.SetNamespace(ns)
			ciliumPols = append(ciliumPols, u)
		}
	}
	if len(netPols) == 0 {
		return "", fmt.Errorf("chart rendered no NetworkPolicy for class %q", class)
	}

	// The chart only renders its CiliumNetworkPolicy when Helm can see the
	// cilium.io/v2 API, which the client-only Template render above cannot. Add
	// it back for the "system" class; applying it is best-effort so non-Cilium
	// clusters are unaffected.
	if class == "system" && len(ciliumPols) == 0 {
		ciliumPols = append(ciliumPols, w.systemCiliumNetworkPolicy(ns, canonicalID))
	}

	// These objects are applied with the raw client, not through Helm, so Helm
	// does not add its ownership metadata. Stamp it ourselves — alongside the
	// class-independent name the chart now renders — so a later `helm upgrade`
	// re-adopts them instead of failing with an ownership conflict, and a
	// `helm uninstall` removes them instead of orphaning them.
	for _, np := range netPols {
		stampHelmOwnership(np, w.Name, ns)
	}
	for _, u := range ciliumPols {
		stampHelmOwnership(u, w.Name, ns)
	}

	selector := helm.LabelCanonicalId + "=" + canonicalID

	npc := w.client.KubeClient().NetworkingV1().NetworkPolicies(ns)
	if err := npc.DeleteCollection(ctx, metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: selector}); err != nil && !k8serrors.IsNotFound(err) {
		return "", fmt.Errorf("failed to delete existing network policies for %s: %w", w.Name, err)
	}

	cnpc := w.client.DynamicClient().Resource(ciliumNetworkPolicyGVR).Namespace(ns)
	if err := cnpc.DeleteCollection(ctx, metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: selector}); err != nil && !k8serrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		// The CRD is absent on non-Cilium clusters; nothing to clean up there.
		w.log.Debug().Err(err).Msg("could not delete CiliumNetworkPolicies (CRD may not be installed)")
	}

	for _, np := range netPols {
		if _, err := npc.Create(ctx, np, metav1.CreateOptions{}); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return "", fmt.Errorf("failed to create network policy %s: %w", np.Name, err)
			}
			existing, gerr := npc.Get(ctx, np.Name, metav1.GetOptions{})
			if gerr != nil {
				return "", fmt.Errorf("failed to get existing network policy %s: %w", np.Name, gerr)
			}
			np.ResourceVersion = existing.ResourceVersion
			if _, uerr := npc.Update(ctx, np, metav1.UpdateOptions{}); uerr != nil {
				return "", fmt.Errorf("failed to update network policy %s: %w", np.Name, uerr)
			}
		}
	}

	for _, u := range ciliumPols {
		_, err := cnpc.Create(ctx, u, metav1.CreateOptions{})
		switch {
		case err == nil:
		case meta.IsNoMatchError(err):
			// Non-Cilium cluster: the plain NetworkPolicy is enough.
			w.log.Debug().Msg("skipping CiliumNetworkPolicy (cilium.io/v2 CRD not installed)")
		case k8serrors.IsAlreadyExists(err):
			if derr := cnpc.Delete(ctx, u.GetName(), metav1.DeleteOptions{}); derr != nil && !k8serrors.IsNotFound(derr) {
				return "", fmt.Errorf("failed to replace CiliumNetworkPolicy %s: %w", u.GetName(), derr)
			}
			if _, cerr := cnpc.Create(ctx, u, metav1.CreateOptions{}); cerr != nil {
				return "", fmt.Errorf("failed to recreate CiliumNetworkPolicy %s: %w", u.GetName(), cerr)
			}
		default:
			return "", fmt.Errorf("failed to create CiliumNetworkPolicy %s: %w", u.GetName(), err)
		}
	}

	// Keep the pod's k8shell.io/network-policy label in sync with the class,
	// and — when the egress shortcuts were replaced — the k8shell.io/egress-rules
	// annotation the workspace-list API reports. A class-only change leaves the
	// blueprint's egress in effect, so its annotation stays correct and is left
	// untouched. Both go in one merge patch.
	patchMeta := map[string]interface{}{
		"labels": map[string]string{helm.LabelNetworkPolicy: class},
	}
	if opts.ReplaceEgress {
		var annVal interface{} // nil clears the annotation via merge patch
		if len(opts.AllowEgressToCIDRs) > 0 || len(opts.AllowEgressToPods) > 0 {
			cidrs := opts.AllowEgressToCIDRs
			if cidrs == nil {
				cidrs = []string{}
			}
			pods := opts.AllowEgressToPods
			if pods == nil {
				pods = []map[string]string{}
			}
			raw, merr := json.Marshal(map[string]interface{}{
				"allowEgressToCIDRs": cidrs,
				"allowEgressToPods":  pods,
			})
			if merr != nil {
				return "", fmt.Errorf("failed to encode egress rules annotation for %s: %w", w.Name, merr)
			}
			annVal = string(raw)
		}
		patchMeta["annotations"] = map[string]interface{}{helm.AnnotationEgressRules: annVal}
	}
	podPatch, err := json.Marshal(map[string]interface{}{"metadata": patchMeta})
	if err != nil {
		return "", fmt.Errorf("failed to build network metadata patch for %s: %w", w.Name, err)
	}
	if _, err := w.client.KubeClient().CoreV1().Pods(ns).Patch(ctx, w.Name, types.MergePatchType,
		podPatch, metav1.PatchOptions{}); err != nil && !k8serrors.IsNotFound(err) {
		w.log.Warn().Err(err).Msgf("failed to update network label/annotations on workspace pod %s", w.Name)
	}

	w.log.Info().Str("workspace", w.Name).Str("class", class).Int("networkPolicies", len(netPols)).
		Int("ciliumNetworkPolicies", len(ciliumPols)).Msg("re-applied workspace network policies")
	return class, nil
}

// Helm's well-known ownership metadata. Helm stamps these on every object it
// installs; objects applied out-of-band must carry them too or the next
// `helm upgrade` rejects the release with an "invalid ownership metadata" error.
const (
	helmManagedByLabel        = "app.kubernetes.io/managed-by"
	helmManagedByValue        = "Helm"
	helmReleaseNameAnnotation = "meta.helm.sh/release-name"
	helmReleaseNsAnnotation   = "meta.helm.sh/release-namespace"
)

// stampHelmOwnership adds Helm's managed-by label and release annotations to obj
// so Helm treats a raw-client-applied object as part of the named release.
func stampHelmOwnership(obj metav1.Object, releaseName, namespace string) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[helmManagedByLabel] = helmManagedByValue
	obj.SetLabels(labels)

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[helmReleaseNameAnnotation] = releaseName
	annotations[helmReleaseNsAnnotation] = namespace
	obj.SetAnnotations(annotations)
}

// systemCiliumNetworkPolicy builds the CiliumNetworkPolicy the k8shell-workspace
// chart renders for the "system" class (see templates/np-system.yaml): the
// workspace endpoint is allowed egress to the "world" and "cluster" entities.
func (w *Workspace) systemCiliumNetworkPolicy(namespace, canonicalID string) *unstructured.Unstructured {
	org := ""
	if w.user != nil {
		org = w.user.Organization
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]interface{}{
			"name":      "cilium-np-" + w.Name,
			"namespace": namespace,
			"labels": map[string]interface{}{
				helm.LabelCanonicalId: canonicalID,
			},
		},
		"spec": map[string]interface{}{
			"endpointSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					helm.LabelOrganization: org,
					helm.LabelCanonicalId:  canonicalID,
				},
			},
			"egress": []interface{}{
				map[string]interface{}{
					"toEntities": []interface{}{"world", "cluster"},
				},
			},
		},
	}}
}
