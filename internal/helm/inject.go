package helm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	// LabelInjected is stamped on a workload's pod template when a workspace is
	// injected, so that GetWorkspaces can discover injected pods cluster-wide.
	LabelInjected = "k8shell.io/injected"

	// AnnotationInjectionState holds a JSON-encoded InjectionState on the workload
	// object, recording everything needed to reverse the injection on eject.
	AnnotationInjectionState = "k8shell.io/injection"

	// LabelWorkloadKind and LabelWorkloadName are stamped onto the injected
	// workload's pod template so the owning workload can be identified directly
	// from any pod label selector, without traversing ReplicaSet owner refs.
	LabelWorkloadKind = "k8shell.io/workload-kind"
	LabelWorkloadName = "k8shell.io/workload-name"

	// Common pod/release labels used across workspace resources.
	LabelUsername     = "k8shell.io/username"
	LabelOrganization = "k8shell.io/organization"
	LabelBlueprint    = "k8shell.io/blueprint"
	LabelCanonicalId  = "k8shell.io/canonical-id"
	LabelJobId        = "k8shell.io/job-id"
	LabelAppVersion   = "k8shell.io/k8shelld-version"
	LabelStorageType  = "k8shell.io/storage-type"
	LabelStorageName  = "k8shell.io/storage-name"
	LabelSubdomain    = "k8shell.io/subdomain"
	LabelInjectTarget = "k8shell.io/inject-target"
	LabelManagedBy    = "k8shell.io/managed-by"

	// AnnotationUserStr holds the base64-encoded canonical user string on a workspace pod.
	AnnotationUserStr = "k8shell.io/userstr"
)

// InjectionState is the structured payload stored in AnnotationInjectionState.
// It records all information needed to cleanly reverse an injection.
type InjectionState struct {
	CanonicalId    string   `json:"canonicalId"`
	Containers     []string `json:"containers,omitempty"`
	InitContainers []string `json:"initContainers,omitempty"`
	Volumes        []string `json:"volumes,omitempty"`
	SharePID       bool     `json:"sharePID"`
}

// ParseInjectionState decodes the injection annotation from an annotation map.
// Returns nil, false if the annotation is absent or malformed.
func ParseInjectionState(annotations map[string]string) (*InjectionState, bool) {
	raw, ok := annotations[AnnotationInjectionState]
	if !ok || raw == "" {
		return nil, false
	}
	var s InjectionState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, false
	}
	return &s, true
}

// InjectedCanonicalId returns the canonical workspace ID recorded in the
// injection annotation, or "" if the workload is not injected.
func InjectedCanonicalId(annotations map[string]string) string {
	s, ok := ParseInjectionState(annotations)
	if !ok {
		return ""
	}
	return s.CanonicalId
}

// InjectionSpec holds the containers, init containers, volumes, and pod-template
// labels/annotations extracted from a rendered workspace chart, ready for injection.
type InjectionSpec struct {
	Containers     []corev1.Container
	InitContainers []corev1.Container
	Volumes        []corev1.Volume
	PodLabels      map[string]string
	// PodAnnotations carries key annotations (e.g. k8shell.io/userstr) from the
	// rendered workspace pod so they can be added to the Deployment's pod template,
	// enabling FindWorkspace/GetWorkspaces to discover injected workspaces by label.
	PodAnnotations map[string]string
}

// WorkspaceResourcesFromTemplate renders the workspace Helm chart and extracts
// all non-Pod YAML documents (ConfigMaps, PVCs, NetworkPolicies, Certs, etc.)
// as raw YAML strings, keyed by "kind/name".
func (c *Client) WorkspaceResourcesFromTemplate(ctx context.Context, values map[string]interface{},
	workspaceCanonicalId string) (map[string]string, error) {
	rendered, err := c.Template(ctx, WORKSPACE_CHART_NAME, InstallOptions{
		ReleaseName: workspaceCanonicalId,
		Values:      values,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render workspace chart: %w", err)
	}

	resources := make(map[string]string)
	for _, doc := range strings.Split(rendered, "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		// Parse just enough to get kind and name.
		var meta struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := sigsyaml.Unmarshal([]byte(doc), &meta); err != nil || meta.Kind == "" {
			continue
		}
		if meta.Kind == "Pod" {
			continue
		}
		key := meta.Kind + "/" + meta.Metadata.Name
		resources[key] = doc
	}
	return resources, nil
}

// InjectionSpecFromTemplate renders the workspace Helm chart, extracts the Pod
// document, and returns its containers/initContainers/volumes renamed with the
// given prefix to avoid conflicts with the target workload's own containers.
func (c *Client) InjectionSpecFromTemplate(ctx context.Context,
	values map[string]interface{}, workspaceCanonicalId string, jobId string,
	sharedPvcPrefix string) (*InjectionSpec, error) {
	rendered, err := c.Template(ctx, WORKSPACE_CHART_NAME, InstallOptions{
		ReleaseName: workspaceCanonicalId,
		Values:      values,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render workspace chart: %w", err)
	}

	var pod corev1.Pod
	found := false
	for _, doc := range strings.Split(rendered, "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" || !strings.Contains(doc, "kind: Pod") {
			continue
		}
		if err := sigsyaml.Unmarshal([]byte(doc), &pod); err != nil {
			continue
		}
		if pod.Kind == "Pod" {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("pod document not found in rendered workspace chart for %s", workspaceCanonicalId)
	}

	prefix := workspaceCanonicalId + "-"

	// Secret-backed volumes are excluded from injection: the referenced secrets
	// are unlikely to exist in the target Deployment's namespace. Volume mounts
	// that reference an excluded volume are also stripped from each container.
	// TODO: make this configurable once the secrets strategy is settled.
	skipVolumes := make(map[string]bool)
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil {
			skipVolumes[v.Name] = true
		}
	}

	// Build a volume name mapping (old → new) for non-skipped volumes only.
	volMap := make(map[string]string, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		if !skipVolumes[v.Name] {
			volMap[v.Name] = prefix + v.Name
		}
	}

	rewriteMounts := func(mounts []corev1.VolumeMount) []corev1.VolumeMount {
		out := make([]corev1.VolumeMount, 0, len(mounts))
		for _, m := range mounts {
			if newName, ok := volMap[m.Name]; ok {
				m.Name = newName
				out = append(out, m)
			}
			// mounts referencing a skipped volume are dropped
		}
		return out
	}

	containers := make([]corev1.Container, len(pod.Spec.Containers))
	for i, c := range pod.Spec.Containers {
		c.Name = prefix + c.Name
		c.VolumeMounts = rewriteMounts(c.VolumeMounts)
		containers[i] = c
	}

	initContainers := make([]corev1.Container, len(pod.Spec.InitContainers))
	for i, c := range pod.Spec.InitContainers {
		c.Name = prefix + c.Name
		c.VolumeMounts = rewriteMounts(c.VolumeMounts)
		initContainers[i] = c
	}

	volumes := make([]corev1.Volume, 0, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		if skipVolumes[v.Name] {
			continue
		}
		v.Name = prefix + v.Name
		if v.PersistentVolumeClaim != nil {
			cn := v.PersistentVolumeClaim.ClaimName
			if strings.HasPrefix(cn, "pvc-") && !strings.HasPrefix(cn, "pvc-"+sharedPvcPrefix) {
				v.PersistentVolumeClaim = v.PersistentVolumeClaim.DeepCopy()
				v.PersistentVolumeClaim.ClaimName = "pvc-" + sharedPvcPrefix + strings.TrimPrefix(cn, "pvc-")
			}
		}
		volumes = append(volumes, v)
	}

	podLabelAllowlist := map[string]bool{
		LabelUsername:     true,
		LabelOrganization: true,
		LabelBlueprint:    true,
	}
	podLabels := make(map[string]string)
	for k, v := range pod.Labels {
		if podLabelAllowlist[k] {
			podLabels[k] = v
		}
	}
	podLabels[LabelCanonicalId] = workspaceCanonicalId
	podLabels[LabelJobId] = jobId

	// Copy key annotations needed for workspace discovery on injected pods.
	podAnnotations := make(map[string]string)
	for _, key := range []string{AnnotationUserStr} {
		if v, ok := pod.Annotations[key]; ok && v != "" {
			podAnnotations[key] = v
		}
	}

	return &InjectionSpec{
		Containers:     containers,
		InitContainers: initContainers,
		Volumes:        volumes,
		PodLabels:      podLabels,
		PodAnnotations: podAnnotations,
	}, nil
}

// InjectIntoWorkload injects the workspace into the workload identified by adapter,
// appending containers, init containers, and volumes from spec, and recording the
// injection in the workload's annotations so it can be reversed by EjectFromWorkload.
// It fails if the workload is already injected with any workspace.
func (c *Client) InjectIntoWorkload(ctx context.Context, adapter WorkloadAdapter,
	workspaceCanonicalId string, spec *InjectionSpec) error {
	// Guard: one injection per workload at a time.
	if existing := InjectedCanonicalId(adapter.GetAnnotations()); existing != "" {
		return fmt.Errorf("%s %s/%s is already injected with workspace %q",
			adapter.Kind(), adapter.Namespace(), adapter.Name(), existing)
	}

	// Collect names for the injection state.
	containerNames := make([]string, len(spec.Containers))
	for i, c := range spec.Containers {
		containerNames[i] = c.Name
	}
	initNames := make([]string, len(spec.InitContainers))
	for i, c := range spec.InitContainers {
		initNames[i] = c.Name
	}
	volumeNames := make([]string, len(spec.Volumes))
	for i, v := range spec.Volumes {
		volumeNames[i] = v.Name
	}

	// Record original shareProcessNamespace so eject can restore it.
	tpl := adapter.GetPodTemplate()
	origSharePID := tpl.Spec.ShareProcessNamespace != nil && *tpl.Spec.ShareProcessNamespace

	// Write a single structured annotation on the workload object.
	state := InjectionState{
		CanonicalId:    workspaceCanonicalId,
		Containers:     containerNames,
		InitContainers: initNames,
		Volumes:        volumeNames,
		SharePID:       origSharePID,
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal injection state: %w", err)
	}
	adapter.SetAnnotation(AnnotationInjectionState, string(stateJSON))

	// Mutate the pod template.
	if tpl.Labels == nil {
		tpl.Labels = make(map[string]string)
	}
	for k, v := range spec.PodLabels {
		tpl.Labels[k] = v
	}
	tpl.Labels[LabelInjected] = "true"
	tpl.Labels[LabelWorkloadKind] = adapter.Kind()
	tpl.Labels[LabelWorkloadName] = adapter.Name()
	if tpl.Annotations == nil {
		tpl.Annotations = make(map[string]string)
	}
	for k, v := range spec.PodAnnotations {
		tpl.Annotations[k] = v
	}
	trueVal := true
	tpl.Spec.ShareProcessNamespace = &trueVal
	tpl.Spec.InitContainers = append(tpl.Spec.InitContainers, spec.InitContainers...)
	tpl.Spec.Containers = append(tpl.Spec.Containers, spec.Containers...)
	tpl.Spec.Volumes = append(tpl.Spec.Volumes, spec.Volumes...)
	adapter.SetPodTemplate(tpl)

	if err := adapter.Update(ctx, c.kubeClient); err != nil {
		return fmt.Errorf("failed to update %s %s/%s: %w", adapter.Kind(), adapter.Namespace(), adapter.Name(), err)
	}

	c.log.Info().Str("kind", adapter.Kind()).Str("namespace", adapter.Namespace()).
		Str("name", adapter.Name()).Str("canonicalId", workspaceCanonicalId).Msg("injected workspace into workload")
	return nil
}

// EjectFromWorkload reverses a previous InjectIntoWorkload call. It reads the
// injection tracking annotations, removes the named containers/initContainers/volumes
// from the pod template, clears the annotations, and returns the canonical ID so
// the caller can delete the associated namespaced resources.
func (c *Client) EjectFromWorkload(ctx context.Context, adapter WorkloadAdapter,
	workspaceCanonicalId string) (string, error) {
	state, ok := ParseInjectionState(adapter.GetAnnotations())
	if !ok || state.CanonicalId != workspaceCanonicalId {
		found := ""
		if ok {
			found = state.CanonicalId
		}
		return "", fmt.Errorf("%s %s/%s is not injected with workspace %q (found %q)",
			adapter.Kind(), adapter.Namespace(), adapter.Name(), workspaceCanonicalId, found)
	}
	canonicalId := state.CanonicalId

	rmContainers := toSet(state.Containers)
	rmInit := toSet(state.InitContainers)
	rmVolumes := toSet(state.Volumes)

	tpl := adapter.GetPodTemplate()

	// Remove injected containers, init containers, volumes.
	tpl.Spec.Containers = filterContainers(tpl.Spec.Containers, rmContainers)
	tpl.Spec.InitContainers = filterContainers(tpl.Spec.InitContainers, rmInit)
	tpl.Spec.Volumes = filterVolumes(tpl.Spec.Volumes, rmVolumes)

	// Remove only the labels injection added (preserve selector-referenced labels).
	injectedLabelKeys := map[string]bool{
		LabelInjected:     true,
		LabelWorkloadKind: true,
		LabelWorkloadName: true,
		LabelCanonicalId:  true,
		LabelJobId:        true,
		LabelUsername:     true,
		LabelOrganization: true,
		LabelBlueprint:    true,
	}
	newLabels := make(map[string]string)
	for k, v := range tpl.Labels {
		if !injectedLabelKeys[k] {
			newLabels[k] = v
		}
	}
	tpl.Labels = newLabels

	// Remove workspace annotations injected into the pod template.
	injectedAnnotationKeys := map[string]bool{
		AnnotationUserStr: true,
	}
	newAnnotations := make(map[string]string)
	for k, v := range tpl.Annotations {
		if !injectedAnnotationKeys[k] {
			newAnnotations[k] = v
		}
	}
	tpl.Annotations = newAnnotations

	// Restore shareProcessNamespace.
	falseVal := false
	restorePID := &falseVal
	if state.SharePID {
		trueVal := true
		restorePID = &trueVal
	}
	tpl.Spec.ShareProcessNamespace = restorePID
	adapter.SetPodTemplate(tpl)

	adapter.DeleteAnnotation(AnnotationInjectionState)

	if err := adapter.Update(ctx, c.kubeClient); err != nil {
		return "", fmt.Errorf("failed to update %s %s/%s during eject: %w", adapter.Kind(), adapter.Namespace(), adapter.Name(), err)
	}

	c.log.Info().Str("kind", adapter.Kind()).Str("namespace", adapter.Namespace()).
		Str("name", adapter.Name()).Str("canonicalId", workspaceCanonicalId).Msg("ejected workspace from workload")
	return canonicalId, nil
}

// ApplyNamespacedResources creates or updates each resource document in the
// given namespace. resources is the map returned by WorkspaceResourcesFromTemplate.
func (c *Client) ApplyNamespacedResources(ctx context.Context, namespace string, resources map[string]string, labels map[string]string) error {
	for key, doc := range resources {
		if err := c.applyGenericResource(ctx, namespace, key, doc, labels); err != nil {
			return err
		}
	}
	return nil
}

// DeleteNamespacedWorkspaceResources deletes all resources in namespace that
// carry the k8shell.io/canonical-id label matching canonicalId.
func (c *Client) DeleteNamespacedWorkspaceResources(ctx context.Context, namespace, canonicalId string) error {
	selector := LabelCanonicalId + "=" + canonicalId
	listOpts := metav1.ListOptions{LabelSelector: selector}
	deleteOpts := metav1.DeleteOptions{}

	kc := c.kubeClient

	if err := kc.CoreV1().ConfigMaps(namespace).DeleteCollection(ctx, deleteOpts, listOpts); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete configmaps for canonical-id %s: %w", canonicalId, err)
	}
	pvcs, err := kc.CoreV1().PersistentVolumeClaims(namespace).List(ctx, listOpts)
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to list PVCs for canonical-id %s: %w", canonicalId, err)
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.Labels[LabelStorageType] == "shared" {
			continue
		}
		if err := kc.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvc.Name, deleteOpts); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete PVC %s for canonical-id %s: %w", pvc.Name, canonicalId, err)
		}
	}
	if err := kc.NetworkingV1().NetworkPolicies(namespace).DeleteCollection(ctx, deleteOpts, listOpts); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete network policies for canonical-id %s: %w", canonicalId, err)
	}

	// Delete cert-manager Certificate resources (CRD — use dynamic client).
	if err := c.dynamicClient.Resource(certGVR).Namespace(namespace).DeleteCollection(
		ctx,
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: selector},
	); err != nil && !k8serrors.IsNotFound(err) {
		c.log.Warn().Err(err).Str("namespace", namespace).Str("canonical-id", canonicalId).
			Msg("failed to delete Certificate resources (cert-manager may not be installed)")
	}

	c.log.Info().Str("namespace", namespace).Str("canonical-id", canonicalId).
		Msg("deleted namespaced workspace resources")
	return nil
}

// applyGenericResource applies a single YAML resource document to the given
// namespace by server-side-applying it via the Kubernetes API.
// For simplicity it currently supports ConfigMaps and PersistentVolumeClaims
// directly; other kinds fall through to a raw JSON patch apply.
func (c *Client) applyGenericResource(ctx context.Context, namespace, key, doc string, extraLabels map[string]string) error {
	// Decode the kind.
	var meta struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name   string            `yaml:"name"`
			Labels map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
	}
	if err := sigsyaml.Unmarshal([]byte(doc), &meta); err != nil {
		return fmt.Errorf("failed to parse resource %s: %w", key, err)
	}

	// Merge extra labels into the document before applying.
	if len(extraLabels) > 0 && meta.Metadata.Labels == nil {
		meta.Metadata.Labels = make(map[string]string)
	}

	switch meta.Kind {
	case "ConfigMap":
		var cm corev1.ConfigMap
		if err := sigsyaml.Unmarshal([]byte(doc), &cm); err != nil {
			return fmt.Errorf("failed to decode ConfigMap %s: %w", meta.Metadata.Name, err)
		}
		cm.Namespace = namespace
		if cm.Labels == nil {
			cm.Labels = make(map[string]string)
		}
		for k, v := range extraLabels {
			cm.Labels[k] = v
		}
		existing, err := c.kubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, cm.Name, metav1.GetOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				return fmt.Errorf("failed to get ConfigMap %s: %w", cm.Name, err)
			}
			if _, err := c.kubeClient.CoreV1().ConfigMaps(namespace).Create(ctx, &cm, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("failed to create ConfigMap %s: %w", cm.Name, err)
			}
		} else {
			cm.ResourceVersion = existing.ResourceVersion
			if _, err := c.kubeClient.CoreV1().ConfigMaps(namespace).Update(ctx, &cm, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("failed to update ConfigMap %s: %w", cm.Name, err)
			}
		}

	case "PersistentVolumeClaim":
		var pvc corev1.PersistentVolumeClaim
		if err := sigsyaml.Unmarshal([]byte(doc), &pvc); err != nil {
			return fmt.Errorf("failed to decode PVC %s: %w", meta.Metadata.Name, err)
		}
		pvc.Namespace = namespace
		if pvc.Labels == nil {
			pvc.Labels = make(map[string]string)
		}
		for k, v := range extraLabels {
			pvc.Labels[k] = v
		}
		_, err := c.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				return fmt.Errorf("failed to get PVC %s: %w", pvc.Name, err)
			}
			if _, err := c.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, &pvc, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("failed to create PVC %s: %w", pvc.Name, err)
			}
		}
		// PVCs are not updated — storage requests are immutable.

	case "Certificate":
		if err := c.applyCertificate(ctx, namespace, doc, extraLabels); err != nil {
			return fmt.Errorf("failed to apply Certificate %s: %w", meta.Metadata.Name, err)
		}

	default:
		c.log.Debug().Str("kind", meta.Kind).Str("name", meta.Metadata.Name).
			Msg("skipping resource kind during injection apply")
	}
	return nil
}

// certGVR is the GroupVersionResource for cert-manager Certificate objects.
var certGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

// applyCertificate applies a cert-manager Certificate resource to the given namespace
// using server-side apply via the dynamic client.
func (c *Client) applyCertificate(ctx context.Context, namespace, doc string, extraLabels map[string]string) error {
	jsonBytes, err := sigsyaml.YAMLToJSON([]byte(doc))
	if err != nil {
		return fmt.Errorf("yaml-json conversion failed: %w", err)
	}

	var obj unstructured.Unstructured
	if err := obj.UnmarshalJSON(jsonBytes); err != nil {
		return fmt.Errorf("unmarshal failed: %w", err)
	}

	obj.SetNamespace(namespace)

	if len(extraLabels) > 0 {
		labels := obj.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		for k, v := range extraLabels {
			labels[k] = v
		}
		obj.SetLabels(labels)
	}

	jsonPatch, err := obj.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal for SSA failed: %w", err)
	}

	_, err = c.dynamicClient.Resource(certGVR).Namespace(namespace).Patch(
		ctx,
		obj.GetName(),
		types.ApplyPatchType,
		jsonPatch,
		metav1.PatchOptions{FieldManager: "k8shell-provisioner", Force: boolPtr(true)},
	)
	return err
}

func boolPtr(b bool) *bool { return &b }

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		if s != "" {
			m[s] = true
		}
	}
	return m
}

func filterContainers(containers []corev1.Container, remove map[string]bool) []corev1.Container {
	out := make([]corev1.Container, 0, len(containers))
	for _, c := range containers {
		if !remove[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

func filterVolumes(volumes []corev1.Volume, remove map[string]bool) []corev1.Volume {
	out := make([]corev1.Volume, 0, len(volumes))
	for _, v := range volumes {
		if !remove[v.Name] {
			out = append(out, v)
		}
	}
	return out
}
