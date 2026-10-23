package clustercapabilities

import (
	"context"
	"time"

	"arally/operator/internal/capabilities"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Coalesce a burst before reconciling the singleton. The mutex-protected dirty
// mask is drained at reconcile start; events arriving during a scan remain queued.
func (r *Reconciler) dependencyHandler() handler.EventHandler {
	enqueue := func(ctx context.Context, object client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
		for _, request := range r.mapDependency(ctx, object) {
			q.AddAfter(request, 250*time.Millisecond)
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, e.Object, q)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, e.ObjectNew, q)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, e.Object, q)
		},
		GenericFunc: func(ctx context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, e.Object, q)
		},
	}
}

func networkDaemonChanges() predicate.Predicate {
	relevant := func(object client.Object) bool { return capabilities.NetworkDaemonSet(object.GetName()) }
	return predicate.And(predicate.NewPredicateFuncs(relevant), predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old, oldOK := e.ObjectOld.(*appsv1.DaemonSet)
		current, currentOK := e.ObjectNew.(*appsv1.DaemonSet)
		return !oldOK || !currentOK || old.Generation != current.Generation ||
			!equality.Semantic.DeepEqual(old.DeletionTimestamp, current.DeletionTimestamp) ||
			old.Status.ObservedGeneration != current.Status.ObservedGeneration ||
			old.Status.DesiredNumberScheduled != current.Status.DesiredNumberScheduled ||
			old.Status.NumberReady != current.Status.NumberReady
	}})
}

func networkConfigChanges() predicate.Predicate {
	relevant := func(object client.Object) bool { return capabilities.NetworkConfigMap(object.GetName()) }
	return predicate.And(predicate.NewPredicateFuncs(relevant), predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old, oldOK := e.ObjectOld.(*corev1.ConfigMap)
		current, currentOK := e.ObjectNew.(*corev1.ConfigMap)
		return !oldOK || !currentOK || !equality.Semantic.DeepEqual(old.Data, current.Data) ||
			!equality.Semantic.DeepEqual(old.DeletionTimestamp, current.DeletionTimestamp)
	}})
}
