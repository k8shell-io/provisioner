package server

import (
	"context"
	"time"

	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// InjectionWatcher watches workloads (Deployment, StatefulSet, DaemonSet) across
// the configured injection namespaces and cleans up workspace resources when a
// workload loses its injection annotation (either updated away or deleted).
type InjectionWatcher struct {
	helmClient   *helm.Client
	namespaces   []string
	log          *zerolog.Logger
	resyncPeriod time.Duration
}

func NewInjectionWatcher(helmClient *helm.Client, namespaces []string) *InjectionWatcher {
	l := log.NewLogger("injection-watcher")
	return &InjectionWatcher{
		helmClient:   helmClient,
		namespaces:   namespaces,
		log:          l,
		resyncPeriod: 10 * time.Minute,
	}
}

// Run starts one set of informers per injection namespace (or cluster-scoped
// informers when namespaces contains "*") and blocks until ctx is cancelled.
func (w *InjectionWatcher) Run(ctx context.Context) {
	if len(w.namespaces) == 0 {
		return
	}

	clusterWide := false
	for _, ns := range w.namespaces {
		if ns == "*" {
			clusterWide = true
			break
		}
	}

	if clusterWide {
		factory := informers.NewSharedInformerFactory(w.helmClient.KubeClient(), w.resyncPeriod)
		w.startWorkloadInformers(ctx, factory, "")
		factory.Start(ctx.Done())
		<-ctx.Done()
		return
	}

	for _, ns := range w.namespaces {
		ns := ns
		factory := informers.NewSharedInformerFactoryWithOptions(
			w.helmClient.KubeClient(),
			w.resyncPeriod,
			informers.WithNamespace(ns),
		)
		w.startWorkloadInformers(ctx, factory, ns)
		factory.Start(ctx.Done())
	}

	<-ctx.Done()
}

// startWorkloadInformers registers one informer per supported workload kind on factory.
// ns is used only for log messages ("" = cluster-wide).
func (w *InjectionWatcher) startWorkloadInformers(ctx context.Context, factory informers.SharedInformerFactory, ns string) {
	for _, kind := range helm.SupportedWorkloadKinds() {
		kind := kind
		// Build a prototype adapter to obtain the informer for this kind.
		proto, err := helm.ProtoAdapter(kind, w.helmClient.KubeClient())
		if err != nil {
			w.log.Error().Str("kind", kind).Err(err).Msg("failed to create prototype adapter")
			continue
		}
		informer := proto.NewInformer(factory)
		if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldMeta, ok1 := oldObj.(metav1.Object)
				newMeta, ok2 := newObj.(metav1.Object)
				if !ok1 || !ok2 {
					return
				}
				oldWorkspace := helm.InjectedCanonicalId(oldMeta.GetAnnotations())
				newWorkspace := helm.InjectedCanonicalId(newMeta.GetAnnotations())
				if oldWorkspace != "" && newWorkspace != oldWorkspace {
					canonicalId := oldWorkspace
					w.cleanup(ctx, oldMeta.GetNamespace(), kind, newMeta.GetName(), oldWorkspace, canonicalId)
				}
			},
			DeleteFunc: func(obj interface{}) {
				meta, ok := obj.(metav1.Object)
				if !ok {
					tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
					if !ok {
						return
					}
					meta, ok = tombstone.Obj.(metav1.Object)
					if !ok {
						return
					}
				}
				workspace := helm.InjectedCanonicalId(meta.GetAnnotations())
				if workspace != "" {
					canonicalId := workspace
					w.cleanup(ctx, meta.GetNamespace(), kind, meta.GetName(), workspace, canonicalId)
				}
			},
		}); err != nil {
			w.log.Error().Str("kind", kind).Err(err).Msg("failed to add event handler")
			continue
		}
		logCtx := w.log.Info().Str("kind", kind)
		if ns != "" {
			logCtx = logCtx.Str("namespace", ns)
		}
		logCtx.Msg("started workload informer")
	}
}

func (w *InjectionWatcher) cleanup(ctx context.Context, namespace, kind, workloadName, workspaceName, canonicalId string) {
	w.log.Info().
		Str("namespace", namespace).
		Str("kind", kind).
		Str("workload", workloadName).
		Str("workspace", workspaceName).
		Str("canonical-id", canonicalId).
		Msg("workload injection removed, cleaning up workspace resources")

	if canonicalId == "" {
		w.log.Warn().
			Str("namespace", namespace).
			Str("workspace", workspaceName).
			Msg("injected-canonical-id annotation missing; cannot delete namespaced resources")
		return
	}

	if err := w.helmClient.DeleteNamespacedWorkspaceResources(ctx, namespace, canonicalId); err != nil {
		w.log.Error().Err(err).
			Str("namespace", namespace).
			Str("canonical-id", canonicalId).
			Msg("failed to delete workspace resources after injection removal")
	}
}
