package agentdefinition

import (
	"context"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const Finalizer = "arally.io/definition-cleanup"

type Reconciler struct {
	client.Client
	Reader client.Reader
	Scheme *runtime.Scheme
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, resultErr error) {
	defer func() { resultErr = diagnostics.SafeError(resultErr) }()
	object := &v1.AgentDefinition{}
	if err := r.Get(ctx, req.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	if object.DeletionTimestamp != nil {
		instances := &v1.AgentInstanceList{}
		if err := reader.List(ctx, instances, client.InNamespace(object.Namespace)); err != nil {
			return ctrl.Result{}, err
		}
		for _, instance := range instances.Items {
			if instance.Spec.DefinitionRef.Name == object.Name || instance.Status.Definition != nil && instance.Status.Definition.UID == object.UID {
				return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, object, false, "DefinitionInUse", "Delete referencing instances before deleting their definition", nil)
			}
		}
		group := &v1.AgentGroup{}
		if err := reader.Get(ctx, client.ObjectKey{Name: object.Spec.AgentGroupRef.Name}, group); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if group.UID != "" && group.NamespaceName() == object.Namespace {
			pending, err := (&allocation.Manager{Client: r.Client, Reader: reader, Scheme: r.Scheme}).RetireDefinition(ctx, group, object.UID)
			if err != nil {
				return ctrl.Result{}, err
			}
			if pending {
				return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, object, false, "CapacityDeleting", "Waiting for managed capacity and native Claims to finish cleanup", nil)
			}
		}
		return ctrl.Result{}, controller.RemoveFinalizer(ctx, r.Client, object, Finalizer)
	}
	if err := controller.AddFinalizer(ctx, r.Client, object, Finalizer); err != nil {
		return ctrl.Result{}, err
	}
	group := &v1.AgentGroup{}
	if err := reader.Get(ctx, client.ObjectKey{Name: object.Spec.AgentGroupRef.Name}, group); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, object, false, "GroupNotFound", "The definition's AgentGroup is unavailable", nil)
		}
		return r.failed(ctx, object, err)
	}
	ready := meta.FindStatusCondition(group.Status.Conditions, "Ready")
	if group.NamespaceName() != object.Namespace || group.DeletionTimestamp != nil || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != group.Generation {
		return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, object, false, "GroupNotReady", "Waiting for the Group namespace and policy", nil)
	}
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: object.Namespace}, ns); err != nil {
		return ctrl.Result{}, err
	}
	if err := controller.CheckOwner(ns, group); err != nil {
		return ctrl.Result{}, err
	}
	revision, err := definition.Publish(ctx, r.Client, reader, r.Scheme, object)
	if err != nil {
		return r.failed(ctx, object, err)
	}
	if err := r.status(ctx, object, true, "ConfigurationPublished", "Immutable configuration published; no allocated execution is started", revision); err != nil {
		return ctrl.Result{}, err
	}
	// Publication depends on watched Group/template changes. Only proactive
	// capacity needs polling for upstream readiness and credential rotation.
	result = ctrl.Result{}
	if policy := object.Spec.Prewarming; policy != nil && policy.Replicas > 0 {
		result.RequeueAfter = controller.DependencyRetry
	}
	return result, r.prepareCapacity(ctx, reader, object, group, revision)
}

func (r *Reconciler) failed(ctx context.Context, object *v1.AgentDefinition, cause error) (ctrl.Result, error) {
	reason, message := diagnostics.Classify(cause)
	if err := r.status(ctx, object, false, reason, message, nil); err != nil {
		return ctrl.Result{}, err
	}
	if diagnostics.IsPending(cause) {
		return ctrl.Result{RequeueAfter: controller.DependencyRetry}, nil
	}
	return ctrl.Result{}, cause
}

func (r *Reconciler) status(ctx context.Context, object *v1.AgentDefinition, ready bool, reason, message string, revision *definition.Revision) error {
	base := object.DeepCopy()
	object.Status.ObservedGeneration = object.Generation
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	} else {
		object.Status.Prewarming = nil
		meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{Type: "Prepared", Status: metav1.ConditionFalse,
			ObservedGeneration: object.Generation, Reason: reason, Message: message})
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, ObservedGeneration: object.Generation, Reason: reason, Message: message})
	if revision != nil {
		object.Status.CurrentRevision, object.Status.TemplateName = revision.ID, revision.Template.GetName()
	}
	if equality.Semantic.DeepEqual(base.Status, object.Status) {
		return nil
	}
	return r.Status().Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client, r.Reader, r.Scheme = mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme()
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1.AgentDefinition{}, definitionDependencyIndex, definitionDependencyKeys); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).For(&v1.AgentDefinition{}).
		Owns(sandbox.ExtensionObject("SandboxTemplate")).
		Watches(&v1.AgentGroup{}, handler.EnqueueRequestsFromMapFunc(r.mapDependency)).
		Watches(sandbox.ExtensionObject("SandboxTemplate"), handler.EnqueueRequestsFromMapFunc(r.mapDependency)).Complete(r)
}
