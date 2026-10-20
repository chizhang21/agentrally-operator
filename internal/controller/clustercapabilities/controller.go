package clustercapabilities

import (
	"context"
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
	"k8s.io/apimachinery/pkg/api/meta"
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
	Reader client.Reader
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
	scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status := capabilities.Discover(scanCtx, r.Reader)
	status.ObservedGeneration = report.Generation
	// Preserve transition times across successful/failed refreshes of the same state.
	conditions := status.Conditions
	status.Conditions = append([]metav1.Condition(nil), report.Status.Conditions...)
	for _, condition := range conditions {
		condition.ObservedGeneration = report.Generation
		meta.SetStatusCondition(&status.Conditions, condition)
	}
	result = ctrl.Result{RequeueAfter: capabilities.RefreshInterval}
	if equality.Semantic.DeepEqual(report.Status, status) {
		return result, nil
	}
	base := report.DeepCopy()
	report.Status = status
	return result, r.Status().Patch(ctx, report, client.MergeFrom(base))
}

// Only the built-in report is bootstrapped. Existing reports and native
// discovery inputs retain their spec and metadata.
func (r *Reconciler) loadReport(ctx context.Context, key client.ObjectKey) (*v1.ClusterCapabilities, error) {
	report := &v1.ClusterCapabilities{}
	if err := r.Get(ctx, key, report); err != nil {
		if !apierrors.IsNotFound(err) || key != (client.ObjectKey{Name: defaultReportName}) {
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

func (r *Reconciler) mapDependency(ctx context.Context, _ client.Object) []reconcile.Request {
	reports := &v1.ClusterCapabilitiesList{}
	if err := r.List(ctx, reports); err != nil {
		ctrl.LoggerFrom(ctx).Error(diagnostics.SafeError(err), "unable to map dependency to ClusterCapabilities")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(reports.Items))
	for _, report := range reports.Items {
		if report.DeletionTimestamp == nil {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&report)})
		}
	}
	return requests
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client, r.Reader = mgr.GetClient(), mgr.GetAPIReader()
	enqueue := handler.EnqueueRequestsFromMapFunc(r.mapDependency)
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
		Watches(&appsv1.DaemonSet{}, enqueue).
		WatchesMetadata(&corev1.ConfigMap{}, enqueue, builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool { return object.GetName() == "cilium-config" }))).
		WatchesMetadata(&corev1.Namespace{}, enqueue, builder.WithPredicates(predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
			return update.ObjectOld.GetLabels()["arally.io/network-discovery"] != update.ObjectNew.GetLabels()["arally.io/network-discovery"]
		}, CreateFunc: func(created event.CreateEvent) bool {
			return created.Object.GetLabels()["arally.io/network-discovery"] == "trusted"
		}, DeleteFunc: func(deleted event.DeleteEvent) bool {
			return deleted.Object.GetLabels()["arally.io/network-discovery"] == "trusted"
		}})).
		Complete(r)
}
