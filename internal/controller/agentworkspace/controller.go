package agentworkspace

import (
	"context"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/workspace"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const instanceUIDIndex = "workspace.instanceUID"

type Reconciler struct {
	client.Client
	Reader client.Reader
}

// Reconcile reports identity and release uncertainty; it performs no data deletion.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	record := &v1.AgentWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, record); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Retained records must remain discoverable after their Instance is gone.
	// Labels are query indexes; the immutable spec remains the identity authority.
	if record.Labels[workspace.GroupUIDLabel] != string(record.Spec.GroupUID) || record.Labels[workspace.NamespaceUIDLabel] != string(record.Spec.NamespaceUID) {
		base := record.DeepCopy()
		if record.Labels == nil {
			record.Labels = map[string]string{}
		}
		record.Labels[workspace.GroupUIDLabel], record.Labels[workspace.NamespaceUIDLabel] = string(record.Spec.GroupUID), string(record.Spec.NamespaceUID)
		if err := r.Patch(ctx, record, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	base := record.DeepCopy()
	instance := &v1.AgentInstance{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: record.Spec.Namespace, Name: record.Spec.InstanceName}, instance)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	releasePending := apierrors.IsNotFound(err) || instance.UID != record.Spec.InstanceUID || instance.DeletionTimestamp != nil
	reason, message := "Bound", "The first Instance binding is retained"
	bindingReady := instance.Status.Workspace != nil && instance.Status.Workspace.WorkspaceUID == record.UID &&
		record.Status.PVCUID != "" && instance.Status.Workspace.UID == record.Status.PVCUID
	if !bindingReady {
		reason, message = "BindingPending", "Waiting for the initial workspace and PVC identity pins"
	}
	if releasePending {
		reason, message = "ReleasePending", "Old execution release is unverified; retention clock, recovery and GC are disabled"
	}
	if record.DeletionTimestamp != nil {
		reason, message = "DeletionBlocked", "Workspace deletion requires a verified release and purge protocol; data is preserved"
	}
	ready := metav1.ConditionFalse
	if record.Status.PVCUID != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		err := reader.Get(ctx, client.ObjectKey{Namespace: record.Spec.Namespace, Name: record.Name}, pvc)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil && pvc.UID == record.Status.PVCUID && pvc.DeletionTimestamp == nil && pvc.Status.Phase == corev1.ClaimBound {
			ready = metav1.ConditionTrue
		}
	}
	bindingStatus := metav1.ConditionFalse
	if bindingReady && !releasePending && record.DeletionTimestamp == nil {
		bindingStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&record.Status.Conditions, metav1.Condition{Type: "BindingReady", Status: bindingStatus, Reason: reason, Message: message, ObservedGeneration: record.Generation})
	meta.SetStatusCondition(&record.Status.Conditions, metav1.Condition{Type: "PVCReady", Status: ready, Reason: "PVCObservation", Message: "PVC readiness is separate from binding and execution release", ObservedGeneration: record.Generation})
	if !equality.Semantic.DeepEqual(base.Status, record.Status) {
		if err := r.Status().Patch(ctx, record, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func workspaceInstanceUID(object client.Object) []string {
	uid := object.(*v1.AgentWorkspace).Spec.InstanceUID
	if uid == "" {
		return nil
	}
	return []string{string(uid)}
}

func (r *Reconciler) mapInstance(ctx context.Context, object client.Object) []reconcile.Request {
	records := &v1.AgentWorkspaceList{}
	if err := r.List(ctx, records, client.MatchingFields{instanceUIDIndex: string(object.GetUID())}); err != nil {
		ctrl.LoggerFrom(ctx).Error(diagnostics.SafeError(err), "Unable to map Instance workspaces")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(records.Items))
	for _, record := range records.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&record)})
	}
	return requests
}

func mapPVC(_ context.Context, object client.Object) []reconcile.Request {
	if object.GetAnnotations()[workspace.UIDAnnotation] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: object.GetName()}}}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client, r.Reader = mgr.GetClient(), mgr.GetAPIReader()
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1.AgentWorkspace{}, instanceUIDIndex, workspaceInstanceUID); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).For(&v1.AgentWorkspace{}).
		Watches(&v1.AgentInstance{}, handler.EnqueueRequestsFromMapFunc(r.mapInstance)).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(mapPVC)).Complete(r)
}
