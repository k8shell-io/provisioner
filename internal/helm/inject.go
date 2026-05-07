package helm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	// AnnotationInjectedWorkspace marks which workspace was injected into a Deployment.
	AnnotationInjectedWorkspace = "k8shell.io/injected-workspace"
	// AnnotationInjectedContainers lists the injected container names (comma-separated).
	AnnotationInjectedContainers = "k8shell.io/injected-containers"
	// AnnotationInjectedInitContainers lists the injected init container names (comma-separated).
	AnnotationInjectedInitContainers = "k8shell.io/injected-init-containers"
	// AnnotationInjectedVolumes lists the injected volume names (comma-separated).
	AnnotationInjectedVolumes = "k8shell.io/injected-volumes"
	// AnnotationInjectedSharePID records the original shareProcessNamespace value
	// before injection so it can be restored on eject.
	AnnotationInjectedSharePID = "k8shell.io/injected-share-pid"
)

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
func (c *Client) WorkspaceResourcesFromTemplate(ctx context.Context, values map[string]interface{}, workspaceName string) (map[string]string, error) {
	rendered, err := c.Template(ctx, WORKSPACE_CHART_NAME, InstallOptions{
		ReleaseName: workspaceName,
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
			continue // Pod spec handled separately via InjectionSpecFromTemplate
		}
		key := meta.Kind + "/" + meta.Metadata.Name
		resources[key] = doc
	}
	return resources, nil
}

// InjectionSpecFromTemplate renders the workspace Helm chart, extracts the Pod
// document, and returns its containers/initContainers/volumes renamed with the
// given prefix to avoid conflicts with the target Deployment's own containers.
func (c *Client) InjectionSpecFromTemplate(ctx context.Context, values map[string]interface{}, workspaceName string) (*InjectionSpec, error) {
	rendered, err := c.Template(ctx, WORKSPACE_CHART_NAME, InstallOptions{
		ReleaseName: workspaceName,
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
		return nil, fmt.Errorf("pod document not found in rendered workspace chart for %s", workspaceName)
	}

	prefix := "k8shell-" + workspaceName + "-"

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
		volumes = append(volumes, v)
	}

	// Only propagate specific k8shell.io/ labels needed for workspace discovery.
	// Helm-managed labels (app.kubernetes.io/*, helm.sh/chart, etc.) and
	// k8shell.io/app must not be carried over: the target Deployment may already
	// have these labels with different values in its selector, which would cause
	// Kubernetes to reject the patch with "selector does not match template labels".
	podLabelAllowlist := map[string]bool{
		"k8shell.io/workspace":      true,
		"k8shell.io/username":       true,
		"k8shell.io/organization":   true,
		"k8shell.io/blueprint":      true,
		"k8shell.io/network-policy": true,
		"k8shell.io/subdomain":      true,
	}
	podLabels := make(map[string]string)
	for k, v := range pod.Labels {
		if podLabelAllowlist[k] {
			podLabels[k] = v
		}
	}

	// Copy key annotations needed for workspace discovery on injected pods.
	podAnnotations := make(map[string]string)
	for _, key := range []string{"k8shell.io/userstr", "workspace.k8shell.io/splash"} {
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

// GetDeployment retrieves a Deployment from the given namespace.
func (c *Client) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	dep, err := c.kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment %s/%s: %w", namespace, name, err)
	}
	return dep, nil
}

// InjectIntoDeployment patches the target Deployment to append the k8shell
// containers, init containers, and volumes from spec, and records the injection
// in the Deployment's annotations so it can be reversed later.
//
// It fails if the Deployment is already injected with any workspace.
func (c *Client) InjectIntoDeployment(ctx context.Context, namespace, deploymentName, workspaceName string, spec *InjectionSpec) error {
	dep, err := c.kubeClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment %s/%s: %w", namespace, deploymentName, err)
	}

	// Guard: one injection per Deployment at a time.
	if existing := dep.Annotations[AnnotationInjectedWorkspace]; existing != "" {
		return fmt.Errorf("deployment %s/%s is already injected with workspace %q; eject it first",
			namespace, deploymentName, existing)
	}

	// Build the strategic merge patch.
	type containerPatch struct {
		Name string `json:"name"`
	}
	type specPatch struct {
		ShareProcessNamespace *bool              `json:"shareProcessNamespace"`
		InitContainers        []corev1.Container `json:"initContainers,omitempty"`
		Containers            []corev1.Container `json:"containers"`
		Volumes               []corev1.Volume    `json:"volumes,omitempty"`
	}
	type templateMeta struct {
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	type templatePatch struct {
		Metadata templateMeta `json:"metadata,omitempty"`
		Spec     specPatch    `json:"spec"`
	}
	type depPatch struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Template templatePatch `json:"template"`
		} `json:"spec"`
	}

	// Collect injected names for the tracking annotations.
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
	origSharePID := dep.Spec.Template.Spec.ShareProcessNamespace
	origSharePIDStr := "false"
	if origSharePID != nil && *origSharePID {
		origSharePIDStr = "true"
	}
	trueVal := true

	var patch depPatch
	patch.Metadata.Annotations = map[string]string{
		AnnotationInjectedWorkspace:      workspaceName,
		AnnotationInjectedContainers:     strings.Join(containerNames, ","),
		AnnotationInjectedInitContainers: strings.Join(initNames, ","),
		AnnotationInjectedVolumes:        strings.Join(volumeNames, ","),
		AnnotationInjectedSharePID:       origSharePIDStr,
	}
	patch.Spec.Template.Metadata = templateMeta{
		Labels:      spec.PodLabels,
		Annotations: spec.PodAnnotations,
	}
	patch.Spec.Template.Spec = specPatch{
		ShareProcessNamespace: &trueVal,
		InitContainers:        spec.InitContainers,
		Containers:            spec.Containers,
		Volumes:               spec.Volumes,
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal injection patch: %w", err)
	}

	_, err = c.kubeClient.AppsV1().Deployments(namespace).Patch(
		ctx, deploymentName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch deployment %s/%s: %w", namespace, deploymentName, err)
	}

	c.log.Info().Str("namespace", namespace).Str("deployment", deploymentName).
		Str("workspace", workspaceName).Msg("injected workspace into deployment")
	return nil
}

// EjectFromDeployment reverses a previous InjectIntoDeployment call. It reads
// the injection tracking annotations from the Deployment, removes the named
// containers/initContainers/volumes from the pod template, and clears the
// annotations.
func (c *Client) EjectFromDeployment(ctx context.Context, namespace, deploymentName, workspaceName string) error {
	dep, err := c.kubeClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil // already gone — nothing to eject
		}
		return fmt.Errorf("failed to get deployment %s/%s: %w", namespace, deploymentName, err)
	}

	ann := dep.Annotations
	if ann[AnnotationInjectedWorkspace] != workspaceName {
		return fmt.Errorf("deployment %s/%s is not injected with workspace %q (found %q)",
			namespace, deploymentName, workspaceName, ann[AnnotationInjectedWorkspace])
	}

	removeSet := func(csv string) map[string]bool {
		m := make(map[string]bool)
		for _, s := range strings.Split(csv, ",") {
			if s != "" {
				m[s] = true
			}
		}
		return m
	}

	rmContainers := removeSet(ann[AnnotationInjectedContainers])
	rmInit := removeSet(ann[AnnotationInjectedInitContainers])
	rmVolumes := removeSet(ann[AnnotationInjectedVolumes])

	// Filter containers, initContainers, volumes and pod labels.
	newContainers := filterContainers(dep.Spec.Template.Spec.Containers, rmContainers)
	newInit := filterContainers(dep.Spec.Template.Spec.InitContainers, rmInit)
	newVolumes := filterVolumes(dep.Spec.Template.Spec.Volumes, rmVolumes)

	newPodLabels := make(map[string]string)
	for k, v := range dep.Spec.Template.Labels {
		if strings.HasPrefix(k, "k8shell.io/") {
			continue
		}
		newPodLabels[k] = v
	}

	// Remove the workspace annotations that were injected into the pod template.
	injectedAnnotationKeys := map[string]bool{
		"k8shell.io/userstr":          true,
		"workspace.k8shell.io/splash": true,
	}
	newPodAnnotations := make(map[string]string)
	for k, v := range dep.Spec.Template.Annotations {
		if !injectedAnnotationKeys[k] {
			newPodAnnotations[k] = v
		}
	}

	// Build a full replacement patch for spec.template (strategic merge patch
	// cannot delete array elements by name — we must supply the complete lists).
	type specPatch struct {
		ShareProcessNamespace *bool              `json:"shareProcessNamespace"`
		InitContainers        []corev1.Container `json:"initContainers"`
		Containers            []corev1.Container `json:"containers"`
		Volumes               []corev1.Volume    `json:"volumes"`
	}
	type templateMeta struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	}
	type templatePatch struct {
		Metadata templateMeta `json:"metadata"`
		Spec     specPatch    `json:"spec"`
	}
	type depPatch struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Template templatePatch `json:"template"`
		} `json:"spec"`
	}

	// Restore shareProcessNamespace to its original value.
	falseVal := false
	restorePID := &falseVal
	if ann[AnnotationInjectedSharePID] == "true" {
		trueVal := true
		restorePID = &trueVal
	}

	var patch depPatch
	// Nullify the tracking annotations (set to empty string; JSON Merge Patch
	// would need null, but StrategicMergePatch with "" effectively clears them).
	patch.Metadata.Annotations = map[string]string{
		AnnotationInjectedWorkspace:      "",
		AnnotationInjectedContainers:     "",
		AnnotationInjectedInitContainers: "",
		AnnotationInjectedVolumes:        "",
		AnnotationInjectedSharePID:       "",
	}
	patch.Spec.Template.Metadata = templateMeta{
		Labels:      newPodLabels,
		Annotations: newPodAnnotations,
	}
	patch.Spec.Template.Spec = specPatch{
		ShareProcessNamespace: restorePID,
		InitContainers:        newInit,
		Containers:            newContainers,
		Volumes:               newVolumes,
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal eject patch: %w", err)
	}

	_, err = c.kubeClient.AppsV1().Deployments(namespace).Patch(
		ctx, deploymentName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch deployment %s/%s during eject: %w", namespace, deploymentName, err)
	}

	c.log.Info().Str("namespace", namespace).Str("deployment", deploymentName).
		Str("workspace", workspaceName).Msg("ejected workspace from deployment")
	return nil
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
// carry the k8shell.io/workspace label matching workspaceName.
func (c *Client) DeleteNamespacedWorkspaceResources(ctx context.Context, namespace, workspaceName string) error {
	selector := "k8shell.io/workspace=" + workspaceName
	listOpts := metav1.ListOptions{LabelSelector: selector}
	deleteOpts := metav1.DeleteOptions{}

	kc := c.kubeClient

	if err := kc.CoreV1().ConfigMaps(namespace).DeleteCollection(ctx, deleteOpts, listOpts); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete configmaps for workspace %s: %w", workspaceName, err)
	}
	if err := kc.CoreV1().PersistentVolumeClaims(namespace).DeleteCollection(ctx, deleteOpts, listOpts); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete PVCs for workspace %s: %w", workspaceName, err)
	}
	if err := kc.NetworkingV1().NetworkPolicies(namespace).DeleteCollection(ctx, deleteOpts, listOpts); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete network policies for workspace %s: %w", workspaceName, err)
	}

	c.log.Info().Str("namespace", namespace).Str("workspace", workspaceName).
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

	default:
		c.log.Debug().Str("kind", meta.Kind).Str("name", meta.Metadata.Name).
			Msg("skipping unsupported resource kind during injection apply")
	}
	return nil
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
