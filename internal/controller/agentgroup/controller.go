package agentgroup

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/capabilities"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// GroupFinalizer guards group-owned namespace and capacity cleanup.
const GroupFinalizer = "arally.io/agent-group-cleanup"

// Reconciler handles AgentGroup namespace policy and shared capacity cleanup.
type Reconciler struct {
	client.Client
	Reader   client.Reader
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, resultErr error) {
	defer func() { resultErr = diagnostics.SafeError(resultErr) }()
	group := &v1.AgentGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if group.DeletionTimestamp != nil {
		return r.finalize(ctx, group)
	}
	if problems := validation.IsDNS1123Label(group.NamespaceName()); len(problems) != 0 || !strings.HasPrefix(group.NamespaceName(), "agentrally-") {
		return ctrl.Result{}, r.status(ctx, group, false, "InvalidConfiguration", "namespace must be a DNS label starting with agentrally-", nil)
	}
	if group.Spec.PodSecurity != "" && group.Spec.PodSecurity != "Restricted" && group.Spec.PodSecurity != "Baseline" {
		return ctrl.Result{}, r.status(ctx, group, false, "InvalidConfiguration", "podSecurity must be Restricted or Baseline", nil)
	}
	if err := controller.AddFinalizer(ctx, r.Client, group, GroupFinalizer); err != nil {
		return ctrl.Result{}, err
	}
	ready, reason, message, err := r.prepare(ctx, group)
	if err != nil {
		if statusErr := r.status(ctx, group, false, "ReconcileError", diagnostics.SafeError(err).Error(), nil); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	var capacityErr error
	if ready {
		pools := allocation.Manager{Client: r.Client, Reader: r.Reader, Scheme: r.Scheme}
		_, capacityErr = pools.Prune(ctx, group)
	}
	if err := r.status(ctx, group, ready, reason, message, capacityErr); err != nil {
		return ctrl.Result{}, err
	}
	if capacityErr != nil {
		return ctrl.Result{}, capacityErr
	}
	return ctrl.Result{RequeueAfter: controller.DependencyRetry}, nil
}

func (r *Reconciler) status(ctx context.Context, group *v1.AgentGroup, ready bool, reason, message string, capacityErr error) error {
	base := group.DeepCopy()
	summary, capacity, err := r.observe(ctx, group)
	if err != nil {
		return err
	}
	group.Status.Summary = summary
	if !ready {
		capacity.Status, capacity.Reason, capacity.Message = metav1.ConditionUnknown, "GroupNotReady", "Waiting for the Group namespace and policy"
	} else if capacityErr != nil {
		capacity.Status, capacity.Reason, capacity.Message = metav1.ConditionFalse, "CapacityReconcileFailed", diagnostics.SafeError(capacityErr).Error()
	}
	meta.SetStatusCondition(&group.Status.Conditions, capacity)
	if group.Spec.NetworkPolicy != nil && group.DeletionTimestamp == nil {
		reader := r.Reader
		if reader == nil {
			reader = r.Client
		}
		validationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		condition := capabilities.NetworkCondition(capabilities.ReadReport(validationCtx, reader), group.Generation)
		cancel()
		meta.SetStatusCondition(&group.Status.Conditions, condition)
	} else {
		meta.RemoveStatusCondition(&group.Status.Conditions, "NetworkPolicySupported")
	}
	group.Status.Namespace, group.Status.ObservedGeneration = group.NamespaceName(), group.Generation
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{Type: controller.ConditionReady, Status: status,
		ObservedGeneration: group.Generation, Reason: reason, Message: message})
	if equality.Semantic.DeepEqual(base.Status, group.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, group, client.MergeFrom(base)); err != nil {
		return err
	}
	controller.RecordConditionChanges(r.Recorder, group, base.Status.Conditions, group.Status.Conditions)
	return nil
}

func (r *Reconciler) finalize(ctx context.Context, group *v1.AgentGroup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(group, GroupFinalizer) {
		return ctrl.Result{}, nil
	}
	ns := &corev1.Namespace{}
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	err := reader.Get(ctx, client.ObjectKey{Name: group.NamespaceName()}, ns)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, controller.RemoveFinalizer(ctx, r.Client, group, GroupFinalizer)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := controller.CheckOwner(ns, group); err != nil {
		return ctrl.Result{}, err
	}
	pools := allocation.Manager{Client: r.Client, Reader: r.Reader, Scheme: r.Scheme}
	if pending, err := pools.Prune(ctx, group); err != nil {
		return ctrl.Result{}, err
	} else if pending {
		return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, group, false, "PoolsDeleting", "Waiting for unused managed pools and templates to finish cleanup", nil)
	}
	workloads := &v1.AgentInstanceList{}
	definitions := &v1.AgentDefinitionList{}
	claims := &corev1.PersistentVolumeClaimList{}
	allocations := sandbox.ExtensionList("SandboxClaim")
	sandboxes := &unstructured.UnstructuredList{}
	sandboxes.SetGroupVersionKind(sandbox.GroupVersion.WithKind("SandboxList"))
	for _, list := range []client.ObjectList{workloads, definitions, claims, sandboxes, allocations} {
		if err := reader.List(ctx, list, client.InNamespace(ns.Name)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if len(workloads.Items)+len(definitions.Items)+len(claims.Items)+len(sandboxes.Items)+len(allocations.Items) != 0 {
		message := fmt.Sprintf("Remove definitions, instances, allocation Claims, Sandboxes and PVCs from %s before deleting the group", ns.Name)
		return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, group, false, "GroupInUse", message, nil)
	}
	done, err := controller.DeleteOwned(ctx, r.Client, ns, group)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, group, false, "Deleting", "Waiting for namespace deletion", nil)
	}
	return ctrl.Result{}, controller.RemoveFinalizer(ctx, r.Client, group, GroupFinalizer)
}

func (r *Reconciler) mapCapabilities(ctx context.Context, object client.Object) []reconcile.Request {
	if object.GetName() != "cluster" {
		return nil
	}
	groups := &v1.AgentGroupList{}
	if err := r.List(ctx, groups); err != nil {
		ctrl.LoggerFrom(ctx).Error(diagnostics.SafeError(err), "unable to map capabilities to AgentGroups")
		return nil
	}
	var requests []reconcile.Request
	for _, group := range groups.Items {
		if group.Spec.NetworkPolicy != nil {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&group)})
		}
	}
	return requests
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client, r.Scheme = mgr.GetClient(), mgr.GetScheme()
	r.Reader = mgr.GetAPIReader()
	r.Recorder = mgr.GetEventRecorder("agentrally-agentgroup")
	return ctrl.NewControllerManagedBy(mgr).For(&v1.AgentGroup{}).
		Owns(&corev1.Namespace{}).Owns(&corev1.ResourceQuota{}).Owns(&corev1.LimitRange{}).Owns(&networkingv1.NetworkPolicy{}).
		Watches(&v1.AgentInstance{}, handler.EnqueueRequestsFromMapFunc(r.mapGroupMember), builder.WithPredicates(groupMemberChanges())).
		Watches(&v1.AgentDefinition{}, handler.EnqueueRequestsFromMapFunc(r.mapGroupMember), builder.WithPredicates(groupMemberChanges())).
		Watches(&v1.ClusterCapabilities{}, handler.EnqueueRequestsFromMapFunc(r.mapCapabilities), builder.WithPredicates(controller.CapabilityChanges())).Complete(r)
}

// Heartbeats and Pod timing observations do not change Group summaries or pool
// references. Keep allocation selection and lifecycle events visible to pruning.
func groupMemberChanges() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
		old, next := update.ObjectOld, update.ObjectNew
		if old == nil || next == nil || old.GetGeneration() != next.GetGeneration() || old.GetUID() != next.GetUID() || !equality.Semantic.DeepEqual(old.GetDeletionTimestamp(), next.GetDeletionTimestamp()) {
			return true
		}
		before, oldInstance := old.(*v1.AgentInstance)
		after, newInstance := next.(*v1.AgentInstance)
		if !oldInstance || !newInstance {
			return false
		}
		return !equality.Semantic.DeepEqual(before.Status.Provisioning, after.Status.Provisioning) ||
			controller.SummarizeInstances([]v1.AgentInstance{*before}) != controller.SummarizeInstances([]v1.AgentInstance{*after})
	}}
}
