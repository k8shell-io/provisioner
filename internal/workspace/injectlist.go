// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	annotationImageSource   = "org.opencontainers.image.source"
	annotationImageRevision = "org.opencontainers.image.revision"
)

// InjectWorkloadInfo describes a workload eligible for workspace injection,
// together with its current injection state.
type InjectWorkloadInfo struct {
	Namespace    string
	Kind         string
	Name         string
	Replicas     int32
	Injected     bool
	Workspace    string
	Username     string
	Organization string
	Blueprint    string
	RepoSource   string
	RepoRevision string
}

// injectedWorkloadInfo is the injection state for a single workload, keyed by
// "<kind>/<name>" and gathered from the injected pod's labels.
type injectedWorkloadInfo struct {
	Workspace    string
	Username     string
	Organization string
	Blueprint    string
}

// ListInjectWorkloads lists workloads eligible for workspace injection across
// the given namespaces and kinds, reporting each workload's current injection
// state. namespaces of ["*"] expands to every namespace currently in the
// cluster. kinds defaults to every supported kind when empty. Results are
// ordered by namespace, then kind, then name.
func ListInjectWorkloads(ctx context.Context, helmClient *helm.Client, namespaces, kinds []string) ([]InjectWorkloadInfo, error) {
	if len(kinds) == 0 {
		kinds = helm.SupportedWorkloadKinds()
	} else {
		normalized := make([]string, len(kinds))
		for i, k := range kinds {
			normalized[i] = strings.ToLower(strings.TrimSpace(k))
			if !slices.Contains(helm.SupportedWorkloadKinds(), normalized[i]) {
				return nil, fmt.Errorf("%w: unsupported workload kind %q", models.ErrInvalidParameters, k)
			}
		}
		kinds = normalized
	}

	kubeClient := helmClient.KubeClient()

	if len(namespaces) == 1 && namespaces[0] == "*" {
		nsList, err := kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %w", err)
		}
		namespaces = make([]string, 0, len(nsList.Items))
		for _, ns := range nsList.Items {
			namespaces = append(namespaces, ns.Name)
		}
	}

	results := make([]InjectWorkloadInfo, 0)
	for _, ns := range namespaces {
		injected, err := injectedWorkloadsInNamespace(ctx, kubeClient, ns)
		if err != nil {
			return nil, err
		}
		for _, kind := range kinds {
			items, err := listWorkloadsOfKind(ctx, kubeClient, ns, kind, injected)
			if err != nil {
				return nil, err
			}
			results = append(results, items...)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Namespace != results[j].Namespace {
			return results[i].Namespace < results[j].Namespace
		}
		if results[i].Kind != results[j].Kind {
			return results[i].Kind < results[j].Kind
		}
		return results[i].Name < results[j].Name
	})

	return results, nil
}

// injectedWorkloadsInNamespace returns the injection state of every injected
// workload in namespace, keyed by "<kind>/<name>", read from the labels
// stamped on the injected pods.
func injectedWorkloadsInNamespace(ctx context.Context, kubeClient kubernetes.Interface, namespace string) (map[string]injectedWorkloadInfo, error) {
	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: helm.LabelInjected + "=true",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list injected pods in namespace %q: %w", namespace, err)
	}

	out := make(map[string]injectedWorkloadInfo, len(pods.Items))
	for _, pod := range pods.Items {
		kind := pod.Labels[helm.LabelWorkloadKind]
		name := pod.Labels[helm.LabelWorkloadName]
		if kind == "" || name == "" {
			continue
		}
		out[kind+" / "+name] = injectedWorkloadInfo{
			Workspace:    pod.Labels[helm.LabelCanonicalId],
			Username:     pod.Labels[helm.LabelUsername],
			Organization: pod.Labels[helm.LabelOrganization],
			Blueprint:    pod.Labels[helm.LabelBlueprint],
		}
	}
	return out, nil
}

// listWorkloadsOfKind lists every workload of kind in namespace, merging in
// injection state from injected.
func listWorkloadsOfKind(ctx context.Context, kubeClient kubernetes.Interface, namespace, kind string,
	injected map[string]injectedWorkloadInfo) ([]InjectWorkloadInfo, error) {

	switch kind {
	case "deployment":
		list, err := kubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list deployments in namespace %q: %w", namespace, err)
		}
		out := make([]InjectWorkloadInfo, 0, len(list.Items))
		for i := range list.Items {
			d := &list.Items[i]
			out = append(out, buildInjectWorkloadInfo(namespace, kind, d.Name,
				replicasOrOne(d.Spec.Replicas), d.Spec.Template.Annotations, injected))
		}
		return out, nil
	case "statefulset":
		list, err := kubeClient.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list statefulsets in namespace %q: %w", namespace, err)
		}
		out := make([]InjectWorkloadInfo, 0, len(list.Items))
		for i := range list.Items {
			s := &list.Items[i]
			out = append(out, buildInjectWorkloadInfo(namespace, kind, s.Name,
				replicasOrOne(s.Spec.Replicas), s.Spec.Template.Annotations, injected))
		}
		return out, nil
	case "daemonset":
		list, err := kubeClient.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list daemonsets in namespace %q: %w", namespace, err)
		}
		out := make([]InjectWorkloadInfo, 0, len(list.Items))
		for i := range list.Items {
			ds := &list.Items[i]
			out = append(out, buildInjectWorkloadInfo(namespace, kind, ds.Name,
				ds.Status.DesiredNumberScheduled, ds.Spec.Template.Annotations, injected))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported workload kind %q", kind)
	}
}

// buildInjectWorkloadInfo assembles an InjectWorkloadInfo from a workload's
// identity, replica count, and pod template annotations, merging in
// injection state from injected when present.
func buildInjectWorkloadInfo(namespace, kind, name string, replicas int32, tplAnnotations map[string]string,
	injected map[string]injectedWorkloadInfo) InjectWorkloadInfo {

	info := InjectWorkloadInfo{
		Namespace:    namespace,
		Kind:         kind,
		Name:         name,
		Replicas:     replicas,
		RepoSource:   tplAnnotations[annotationImageSource],
		RepoRevision: tplAnnotations[annotationImageRevision],
	}
	if inj, ok := injected[kind+" / "+name]; ok {
		info.Injected = true
		info.Workspace = inj.Workspace
		info.Username = inj.Username
		info.Organization = inj.Organization
		info.Blueprint = inj.Blueprint
	}
	return info
}

func replicasOrOne(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

