package helm

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// WorkloadAdapter abstracts over Deployment, StatefulSet, and DaemonSet so that
// inject/eject logic and the watcher can work with any supported kind uniformly.
// New kinds (including CRDs via custom adapters) implement this interface.
type WorkloadAdapter interface {
	// Identity
	Kind() string
	Name() string
	Namespace() string

	// Annotations on the workload object (used for injection tracking).
	GetAnnotations() map[string]string
	SetAnnotation(key, value string)
	DeleteAnnotation(key string)

	// Pod template — containers, volumes, labels, annotations.
	GetPodTemplate() *corev1.PodTemplateSpec
	SetPodTemplate(tpl *corev1.PodTemplateSpec)

	// Selector is needed to ensure labels required by the selector are not removed.
	GetSelector() *metav1.LabelSelector

	// Update persists the in-memory changes back to the cluster.
	Update(ctx context.Context, client kubernetes.Interface) error

	// NewInformer returns a SharedIndexInformer scoped to this kind.
	// The watcher calls this for each registered adapter to build its informer set.
	NewInformer(factory informers.SharedInformerFactory) cache.SharedIndexInformer
}

// GetWorkloadAdapter fetches the named workload of the given kind from the cluster
// and returns the appropriate adapter. Kind matching is case-insensitive; name
// is lowercased before the lookup (Kubernetes names are always lowercase).
func (c *Client) GetWorkloadAdapter(ctx context.Context, namespace, kind, name string) (WorkloadAdapter, error) {
	kind = normalizeKind(kind)
	name = strings.ToLower(name)
	switch kind {
	case "Deployment":
		dep, err := c.kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get Deployment %s/%s: %w", namespace, name, err)
		}
		return &DeploymentAdapter{dep: dep}, nil
	case "StatefulSet":
		ss, err := c.kubeClient.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get StatefulSet %s/%s: %w", namespace, name, err)
		}
		return &StatefulSetAdapter{ss: ss}, nil
	case "DaemonSet":
		ds, err := c.kubeClient.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get DaemonSet %s/%s: %w", namespace, name, err)
		}
		return &DaemonSetAdapter{ds: ds}, nil
	default:
		return nil, fmt.Errorf("unsupported workload kind %q; supported: Deployment, StatefulSet, DaemonSet", kind)
	}
}

// SupportedWorkloadKinds returns the list of workload kinds the adapter factory supports.
func SupportedWorkloadKinds() []string {
	return []string{"Deployment", "StatefulSet", "DaemonSet"}
}

// normalizeKind converts a workload kind string to its canonical title-case form
// (e.g. "deployment" → "Deployment") so callers don't need to match case exactly.
func normalizeKind(kind string) string {
	switch strings.ToLower(kind) {
	case "deployment":
		return "deployment"
	case "statefulset":
		return "statefulset"
	case "daemonset":
		return "daemonset"
	}
	return kind // preserve unknown kinds so the switch default can report them
}

// ProtoAdapter returns a zero-value adapter for kind that can be used to call
// NewInformer without needing a real cluster object. kubeClient is not used
// here but is kept in the signature for symmetry with GetWorkloadAdapter.
func ProtoAdapter(kind string, _ kubernetes.Interface) (WorkloadAdapter, error) {
	kind = normalizeKind(kind)
	switch kind {
	case "Deployment":
		return &DeploymentAdapter{dep: &appsv1.Deployment{}}, nil
	case "StatefulSet":
		return &StatefulSetAdapter{ss: &appsv1.StatefulSet{}}, nil
	case "DaemonSet":
		return &DaemonSetAdapter{ds: &appsv1.DaemonSet{}}, nil
	default:
		return nil, fmt.Errorf("unsupported workload kind %q", kind)
	}
}

// --- DeploymentAdapter ---

type DeploymentAdapter struct {
	dep *appsv1.Deployment
}

func (a *DeploymentAdapter) Kind() string      { return "Deployment" }
func (a *DeploymentAdapter) Name() string      { return a.dep.Name }
func (a *DeploymentAdapter) Namespace() string { return a.dep.Namespace }

func (a *DeploymentAdapter) GetAnnotations() map[string]string {
	if a.dep.Annotations == nil {
		a.dep.Annotations = make(map[string]string)
	}
	return a.dep.Annotations
}
func (a *DeploymentAdapter) SetAnnotation(key, value string) {
	if a.dep.Annotations == nil {
		a.dep.Annotations = make(map[string]string)
	}
	a.dep.Annotations[key] = value
}
func (a *DeploymentAdapter) DeleteAnnotation(key string) {
	delete(a.dep.Annotations, key)
}
func (a *DeploymentAdapter) GetPodTemplate() *corev1.PodTemplateSpec {
	return &a.dep.Spec.Template
}
func (a *DeploymentAdapter) SetPodTemplate(tpl *corev1.PodTemplateSpec) {
	a.dep.Spec.Template = *tpl
}
func (a *DeploymentAdapter) GetSelector() *metav1.LabelSelector {
	return a.dep.Spec.Selector
}
func (a *DeploymentAdapter) Update(ctx context.Context, client kubernetes.Interface) error {
	_, err := client.AppsV1().Deployments(a.dep.Namespace).Update(ctx, a.dep, metav1.UpdateOptions{})
	return err
}
func (a *DeploymentAdapter) NewInformer(factory informers.SharedInformerFactory) cache.SharedIndexInformer {
	return factory.Apps().V1().Deployments().Informer()
}

// --- StatefulSetAdapter ---

type StatefulSetAdapter struct {
	ss *appsv1.StatefulSet
}

func (a *StatefulSetAdapter) Kind() string      { return "StatefulSet" }
func (a *StatefulSetAdapter) Name() string      { return a.ss.Name }
func (a *StatefulSetAdapter) Namespace() string { return a.ss.Namespace }

func (a *StatefulSetAdapter) GetAnnotations() map[string]string {
	if a.ss.Annotations == nil {
		a.ss.Annotations = make(map[string]string)
	}
	return a.ss.Annotations
}
func (a *StatefulSetAdapter) SetAnnotation(key, value string) {
	if a.ss.Annotations == nil {
		a.ss.Annotations = make(map[string]string)
	}
	a.ss.Annotations[key] = value
}
func (a *StatefulSetAdapter) DeleteAnnotation(key string) {
	delete(a.ss.Annotations, key)
}
func (a *StatefulSetAdapter) GetPodTemplate() *corev1.PodTemplateSpec {
	return &a.ss.Spec.Template
}
func (a *StatefulSetAdapter) SetPodTemplate(tpl *corev1.PodTemplateSpec) {
	a.ss.Spec.Template = *tpl
}
func (a *StatefulSetAdapter) GetSelector() *metav1.LabelSelector {
	return a.ss.Spec.Selector
}
func (a *StatefulSetAdapter) Update(ctx context.Context, client kubernetes.Interface) error {
	_, err := client.AppsV1().StatefulSets(a.ss.Namespace).Update(ctx, a.ss, metav1.UpdateOptions{})
	return err
}
func (a *StatefulSetAdapter) NewInformer(factory informers.SharedInformerFactory) cache.SharedIndexInformer {
	return factory.Apps().V1().StatefulSets().Informer()
}

// --- DaemonSetAdapter ---

type DaemonSetAdapter struct {
	ds *appsv1.DaemonSet
}

func (a *DaemonSetAdapter) Kind() string      { return "DaemonSet" }
func (a *DaemonSetAdapter) Name() string      { return a.ds.Name }
func (a *DaemonSetAdapter) Namespace() string { return a.ds.Namespace }

func (a *DaemonSetAdapter) GetAnnotations() map[string]string {
	if a.ds.Annotations == nil {
		a.ds.Annotations = make(map[string]string)
	}
	return a.ds.Annotations
}
func (a *DaemonSetAdapter) SetAnnotation(key, value string) {
	if a.ds.Annotations == nil {
		a.ds.Annotations = make(map[string]string)
	}
	a.ds.Annotations[key] = value
}
func (a *DaemonSetAdapter) DeleteAnnotation(key string) {
	delete(a.ds.Annotations, key)
}
func (a *DaemonSetAdapter) GetPodTemplate() *corev1.PodTemplateSpec {
	return &a.ds.Spec.Template
}
func (a *DaemonSetAdapter) SetPodTemplate(tpl *corev1.PodTemplateSpec) {
	a.ds.Spec.Template = *tpl
}
func (a *DaemonSetAdapter) GetSelector() *metav1.LabelSelector {
	return a.ds.Spec.Selector
}
func (a *DaemonSetAdapter) Update(ctx context.Context, client kubernetes.Interface) error {
	_, err := client.AppsV1().DaemonSets(a.ds.Namespace).Update(ctx, a.ds, metav1.UpdateOptions{})
	return err
}
func (a *DaemonSetAdapter) NewInformer(factory informers.SharedInformerFactory) cache.SharedIndexInformer {
	return factory.Apps().V1().DaemonSets().Informer()
}
