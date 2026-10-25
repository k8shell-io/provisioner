// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ReleasePod is the part of a deployed Helm release that the read-only
// workspace lookups need: the release Secret's metadata (whose labels carry
// the workspace's username/organization/blueprint) plus the Pod decoded from
// the release's stored manifest. Pod is nil when the release has no Pod in
// its manifest (i.e. it isn't a workspace release) or failed to decode.
//
// It is what ListDeployedReleasePodsCached returns, and also the object the
// release-secret informer stores in place of the Secret itself: a release
// Secret carries the whole chart (templates included) and decodes to ~65 KB
// of JSON, so decoding it once per change and keeping only this is what makes
// listing hundreds of workspaces cheap.
type ReleasePod struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Pod           *corev1.Pod
	FirstDeployed time.Time
	LastDeployed  time.Time
}

// DeepCopyObject implements runtime.Object so ReleasePod can live in an
// informer store.
func (r *ReleasePod) DeepCopyObject() runtime.Object {
	out := &ReleasePod{
		TypeMeta:      r.TypeMeta,
		FirstDeployed: r.FirstDeployed,
		LastDeployed:  r.LastDeployed,
	}
	r.DeepCopyInto(&out.ObjectMeta)
	if r.Pod != nil {
		out.Pod = r.Pod.DeepCopy()
	}
	return out
}

// releasePodFromSecret decodes a Helm release Secret into a ReleasePod. A
// Secret that fails to decode is logged and returned with a nil Pod, so a
// single malformed release never takes down a whole listing.
func releasePodFromSecret(log *zerolog.Logger, s *corev1.Secret) *ReleasePod {
	rp := &ReleasePod{ObjectMeta: *s.ObjectMeta.DeepCopy()}
	rp.ManagedFields = nil

	rel, err := decodeHelmReleaseSecret(s.Data["release"])
	if err != nil {
		log.Warn().Err(err).Str("secret", s.Name).Msg("failed to decode Helm release secret")
		return rp
	}
	if rel.Info != nil {
		rp.FirstDeployed = rel.Info.FirstDeployed.Time
		rp.LastDeployed = rel.Info.LastDeployed.Time
	}
	// A release without a Pod in its manifest isn't a workspace release; it
	// is kept (with a nil Pod) only so the store mirrors the Secret listing.
	if pod, err := PodManifestFromRelease(rel); err == nil {
		rp.Pod = pod
	}
	return rp
}

// workspaceInformers holds the watch-backed caches serving the read-only
// workspace lookups: every pod in the target namespace, the target
// namespace's deployed Helm release Secrets (stored as ReleasePods), and the
// injected workspace pods in the injection namespaces. Until an informer has
// synced, lookups it would serve fall back to the TTL cache.
type workspaceInformers struct {
	targetNamespace string
	targetPods      cache.SharedIndexInformer
	releases        cache.SharedIndexInformer

	// injectedPods is keyed by namespace, with "" for a single cluster-wide
	// informer (injectNamespaces ["*"]). Each lists only pods labelled
	// LabelInjected=true.
	injectedPods map[string]cache.SharedIndexInformer
}

// StartWorkspaceInformers starts the informers backing ListPodsCached and
// ListDeployedReleasePodsCached for the target namespace and, if any, the
// injection namespaces ("*" for cluster-wide). It returns immediately: until
// each informer finishes its initial list, the lookups it would serve keep
// using the TTL cache. The informers stop when ctx is cancelled.
func (c *Client) StartWorkspaceInformers(ctx context.Context, injectNamespaces []string) {
	inf := &workspaceInformers{
		targetNamespace: c.targetNamespace,
		targetPods:      newPodInformer(c.kubeClient, c.targetNamespace, ""),
		releases:        newReleasePodInformer(c.log, c.kubeClient, c.targetNamespace),
		injectedPods:    map[string]cache.SharedIndexInformer{},
	}

	injectedSelector := LabelInjected + "=true"
	for _, ns := range injectNamespaces {
		if ns == "*" {
			inf.injectedPods = map[string]cache.SharedIndexInformer{
				"": newPodInformer(c.kubeClient, "", injectedSelector),
			}
			break
		}
		inf.injectedPods[ns] = newPodInformer(c.kubeClient, ns, injectedSelector)
	}

	all := []cache.SharedIndexInformer{inf.targetPods, inf.releases}
	for _, i := range inf.injectedPods {
		all = append(all, i)
	}
	synced := make([]cache.InformerSynced, 0, len(all))
	for _, i := range all {
		go i.Run(ctx.Done())
		synced = append(synced, i.HasSynced)
	}
	c.informers.Store(inf)

	go func() {
		start := time.Now()
		if cache.WaitForCacheSync(ctx.Done(), synced...) {
			c.log.Info().Dur("took", time.Since(start)).Msg("Workspace informers synced")
		}
	}()
}

func newPodInformer(client kubernetes.Interface, namespace, selector string) cache.SharedIndexInformer {
	inf := coreinformers.NewFilteredPodInformer(client, namespace, 0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		func(opts *metav1.ListOptions) { opts.LabelSelector = selector })
	_ = inf.SetTransform(func(obj interface{}) (interface{}, error) {
		if p, ok := obj.(*corev1.Pod); ok {
			p.ManagedFields = nil
		}
		return obj, nil
	})
	return inf
}

func newReleasePodInformer(log *zerolog.Logger, client kubernetes.Interface, namespace string) cache.SharedIndexInformer {
	inf := coreinformers.NewFilteredSecretInformer(client, namespace, 0, cache.Indexers{},
		func(opts *metav1.ListOptions) { opts.LabelSelector = releaseSecretSelector("") })
	// Idempotent: anything that isn't a Secret (an already-transformed
	// ReleasePod) is passed through untouched.
	_ = inf.SetTransform(func(obj interface{}) (interface{}, error) {
		if s, ok := obj.(*corev1.Secret); ok {
			return releasePodFromSecret(log, s), nil
		}
		return obj, nil
	})
	return inf
}

// listPods serves a pod listing from a synced informer covering namespace
// and selector, reporting false when none does. A selector is only served by
// an injected-pods informer when it itself requires LabelInjected=true, since
// those informers hold nothing else.
func (w *workspaceInformers) listPods(namespace, selector string) ([]corev1.Pod, bool, error) {
	sel, err := k8slabels.Parse(selector)
	if err != nil {
		return nil, false, fmt.Errorf("unable to parse selector %q: %w", selector, err)
	}

	var inf cache.SharedIndexInformer
	switch {
	case namespace == w.targetNamespace:
		inf = w.targetPods
	case requiresInjected(sel):
		if i, ok := w.injectedPods[namespace]; ok {
			inf = i
		} else if i, ok := w.injectedPods[""]; ok {
			inf = i
		}
	}
	if inf == nil || !inf.HasSynced() {
		return nil, false, nil
	}

	var objs []interface{}
	if namespace == "" {
		objs = inf.GetStore().List()
	} else if objs, err = inf.GetIndexer().ByIndex(cache.NamespaceIndex, namespace); err != nil {
		return nil, false, err
	}

	pods := make([]corev1.Pod, 0, len(objs))
	for _, o := range objs {
		p := o.(*corev1.Pod)
		if sel.Matches(k8slabels.Set(p.Labels)) {
			pods = append(pods, *p)
		}
	}
	return pods, true, nil
}

// listReleasePods serves ListDeployedReleasePodsCached from the release
// informer, reporting false when it doesn't cover namespace or hasn't synced.
func (w *workspaceInformers) listReleasePods(namespace string) ([]*ReleasePod, bool) {
	if namespace != w.targetNamespace || !w.releases.HasSynced() {
		return nil, false
	}
	objs := w.releases.GetStore().List()
	out := make([]*ReleasePod, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(*ReleasePod))
	}
	return out, true
}

func requiresInjected(sel k8slabels.Selector) bool {
	reqs, _ := sel.Requirements()
	for _, r := range reqs {
		if r.Key() != LabelInjected {
			continue
		}
		switch r.Operator() {
		case selection.Equals, selection.DoubleEquals, selection.In:
			if r.Values().Len() == 1 && r.Values().Has("true") {
				return true
			}
		}
	}
	return false
}
