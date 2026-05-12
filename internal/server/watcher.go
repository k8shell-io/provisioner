package server

import (
	"context"
	"time"

	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// InjectionWatcher watches Deployments across the configured injection namespaces
// and cleans up workspace resources when a Deployment loses its injection annotation
// (either updated away or deleted).
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

// Run starts one informer per injection namespace (or a single cluster-scoped
// informer when namespaces contains "*") and blocks until ctx is cancelled.
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
		w.startDeploymentInformer(ctx, factory)
		factory.Start(ctx.Done())
		go func() {
			if !cache.WaitForCacheSync(ctx.Done(), factory.Apps().V1().Deployments().Informer().HasSynced) {
				w.log.Error().Msg("cluster-wide deployment informer cache sync timed out")
			} else {
				w.log.Info().Msg("cluster-wide deployment informer synced")
			}
		}()
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
		w.startDeploymentInformer(ctx, factory)
		factory.Start(ctx.Done())
		go func(ns string) {
			if !cache.WaitForCacheSync(ctx.Done(), factory.Apps().V1().Deployments().Informer().HasSynced) {
				w.log.Error().Str("namespace", ns).Msg("deployment informer cache sync timed out")
			} else {
				w.log.Info().Str("namespace", ns).Msg("deployment informer synced")
			}
		}(ns)
	}

	<-ctx.Done()
}

func (w *InjectionWatcher) startDeploymentInformer(ctx context.Context, factory informers.SharedInformerFactory) {
	depInformer := factory.Apps().V1().Deployments().Informer()
	depInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDep, ok := oldObj.(*appsv1.Deployment)
			if !ok {
				return
			}
			newDep, ok := newObj.(*appsv1.Deployment)
			if !ok {
				return
			}
			oldWorkspace := oldDep.Annotations[helm.AnnotationInjectedWorkspace]
			newWorkspace := newDep.Annotations[helm.AnnotationInjectedWorkspace]
			if oldWorkspace != "" && newWorkspace != oldWorkspace {
				canonicalId := oldDep.Annotations[helm.AnnotationInjectedCanonicalId]
				w.cleanup(ctx, oldDep.Namespace, newDep.Name, oldWorkspace, canonicalId)
			}
		},
		DeleteFunc: func(obj interface{}) {
			dep, ok := obj.(*appsv1.Deployment)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				dep, ok = tombstone.Obj.(*appsv1.Deployment)
				if !ok {
					return
				}
			}
			workspace := dep.Annotations[helm.AnnotationInjectedWorkspace]
			if workspace != "" {
				canonicalId := dep.Annotations[helm.AnnotationInjectedCanonicalId]
				w.cleanup(ctx, dep.Namespace, dep.Name, workspace, canonicalId)
			}
		},
	})
}

func (w *InjectionWatcher) cleanup(ctx context.Context, namespace, deploymentName, workspaceName, canonicalId string) {
	w.log.Info().
		Str("namespace", namespace).
		Str("deployment", deploymentName).
		Str("workspace", workspaceName).
		Str("canonical-id", canonicalId).
		Msg("deployment injection removed, cleaning up workspace resources")

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
