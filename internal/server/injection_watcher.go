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

// Run starts one informer per injection namespace and blocks until ctx is cancelled.
func (w *InjectionWatcher) Run(ctx context.Context) {
	if len(w.namespaces) == 0 {
		return
	}

	for _, ns := range w.namespaces {
		ns := ns
		factory := informers.NewSharedInformerFactoryWithOptions(
			w.helmClient.KubeClient(),
			w.resyncPeriod,
			informers.WithNamespace(ns),
		)
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
					w.cleanup(ctx, ns, newDep.Name, oldWorkspace)
				}
			},
			DeleteFunc: func(obj interface{}) {
				dep, ok := obj.(*appsv1.Deployment)
				if !ok {
					// handle tombstone
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
					w.cleanup(ctx, ns, dep.Name, workspace)
				}
			},
		})

		factory.Start(ctx.Done())

		go func(ns string) {
			if !cache.WaitForCacheSync(ctx.Done(), depInformer.HasSynced) {
				w.log.Error().Str("namespace", ns).Msg("deployment informer cache sync timed out")
			} else {
				w.log.Info().Str("namespace", ns).Msg("deployment informer synced")
			}
		}(ns)
	}

	<-ctx.Done()
}

func (w *InjectionWatcher) cleanup(ctx context.Context, namespace, deploymentName, workspaceName string) {
	w.log.Info().
		Str("namespace", namespace).
		Str("deployment", deploymentName).
		Str("workspace", workspaceName).
		Msg("deployment injection removed, cleaning up workspace resources")

	if err := w.helmClient.DeleteNamespacedWorkspaceResources(ctx, namespace, workspaceName); err != nil {
		w.log.Error().Err(err).
			Str("namespace", namespace).
			Str("workspace", workspaceName).
			Msg("failed to delete workspace resources after injection removal")
	}
}
