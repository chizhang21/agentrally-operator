package clustercapabilities

import (
	"context"
	"sync"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/capabilities"
	"arally/operator/internal/diagnostics"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const defaultReportName = "cluster"

// Heartbeats, image inventories and unrelated status updates do not change discovery.
func nodeDiscoveryChanges() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
		old, oldOK := update.ObjectOld.(*corev1.Node)
		current, currentOK := update.ObjectNew.(*corev1.Node)
		if !oldOK || !currentOK {
			return true
		}
		ready := func(node *corev1.Node) bool {
			for _, condition := range node.Status.Conditions {
				if condition.Type == corev1.NodeReady {
					return condition.Status == corev1.ConditionTrue
				}
			}
			return false
		}
		return !equality.Semantic.DeepEqual(old.Labels, current.Labels) ||
			!equality.Semantic.DeepEqual(old.Spec, current.Spec) ||
			!equality.Semantic.DeepEqual(old.DeletionTimestamp, current.DeletionTimestamp) ||
			old.Status.NodeInfo.ContainerRuntimeVersion != current.Status.NodeInfo.ContainerRuntimeVersion || ready(old) != ready(current)
	}}
}

// Reconciler publishes read-only cluster capability evidence.
type Reconciler struct {
	client.Client
	// Reader bypasses the informer cache so timestamps reflect actual API reads.
	Reader  client.Reader
	Now     func() time.Time
	mu      sync.Mutex
	dirty   capabilities.Section
	started bool
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, resultErr error) {
	defer func() { resultErr = diagnostics.SafeError(resultErr) }()
	report, err := r.loadReport(ctx, req.NamespacedName)
	if err != nil || report == nil {
		return ctrl.Result{}, err
	}
	if report.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	base := report.DeepCopy()
	r.mu.Lock()
	dirty := r.dirty
	r.dirty = 0
	r.mu.Unlock()
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	observer := capabilities.Observer{Reader: reader, Cache: r.Client, Now: r.Now}
	delay := observer.Refresh(ctx, report, dirty, !r.started)
	if !equality.Semantic.DeepEqual(base.Status, report.Status) {
		if err := r.Status().Patch(ctx, report, client.MergeFrom(base)); err != nil {
			r.mu.Lock()
			r.dirty |= dirty
			r.mu.Unlock()
			return ctrl.Result{}, err
		}
	}
	r.started = true
	return ctrl.Result{RequeueAfter: delay}, nil
}

// Only the built-in report is bootstrapped. Existing reports and native
// discovery inputs retain their spec and metadata.
func (r *Reconciler) loadReport(ctx context.Context, key client.ObjectKey) (*v1.ClusterCapabilities, error) {
	if key != (client.ObjectKey{Name: defaultReportName}) {
		return nil, nil
	}
	report := &v1.ClusterCapabilities{}
	if err := r.Get(ctx, key, report); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, client.IgnoreNotFound(err)
		}
		report.Name = defaultReportName
		if err := r.Create(ctx, report); err != nil {
			// A concurrent creator or a cache lag is retried by the work queue.
			return nil, err
		}
	}
	return report, nil
}

func (r *Reconciler) mapDependency(_ context.Context, object client.Object) []reconcile.Request {
	var section capabilities.Section
	switch object.(type) {
	case *corev1.Node, *nodev1.RuntimeClass:
		section = capabilities.Runtime
	case *storagev1.StorageClass, *storagev1.CSIDriver, *storagev1.CSINode:
		section = capabilities.Storage
	case *appsv1.DaemonSet, *corev1.ConfigMap, *corev1.Namespace:
		section = capabilities.Network
	}
	r.mu.Lock()
	r.dirty |= section
	r.mu.Unlock()
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: defaultReportName}}}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client, r.Reader = mgr.GetClient(), mgr.GetAPIReader()
	enqueue := r.dependencyHandler()
	// Seed the elected controller's queue even on a fresh cluster with no reports.
	startup := make(chan event.GenericEvent, 1)
	startup <- event.GenericEvent{Object: &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Name: defaultReportName}}}
	close(startup)
	// Exclude our own status writes, which would otherwise create a scan/write loop.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ClusterCapabilities{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(startup, &handler.EnqueueRequestForObject{})).
		Watches(&corev1.Node{}, enqueue, builder.WithPredicates(nodeDiscoveryChanges())).
		Watches(&nodev1.RuntimeClass{}, enqueue).
		Watches(&storagev1.StorageClass{}, enqueue).
		Watches(&storagev1.CSIDriver{}, enqueue).
		Watches(&storagev1.CSINode{}, enqueue).
		Watches(&appsv1.DaemonSet{}, enqueue, builder.WithPredicates(networkDaemonChanges())).
		Watches(&corev1.ConfigMap{}, enqueue, builder.WithPredicates(networkConfigChanges())).
		Watches(&corev1.Namespace{}, enqueue, builder.WithPredicates(predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
			return update.ObjectOld.GetLabels()["arally.io/network-discovery"] != update.ObjectNew.GetLabels()["arally.io/network-discovery"]
		}, CreateFunc: func(created event.CreateEvent) bool {
			return created.Object.GetLabels()["arally.io/network-discovery"] == "trusted"
		}, DeleteFunc: func(deleted event.DeleteEvent) bool {
			return deleted.Object.GetLabels()["arally.io/network-discovery"] == "trusted"
		}})).
		Complete(r)
}
