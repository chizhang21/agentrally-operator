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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
			if instance.Spec.DefinitionRef.Name == object.Name {
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
		return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, object, false, "GroupNotFound", "The definition's AgentGroup is unavailable", nil)
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
		reason, message := diagnostics.Classify(err)
		return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, object, false, reason, message, nil)
	}
	return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.status(ctx, object, true, "ConfigurationPublished", "Immutable configuration published; no allocated execution is started", revision)
}

func (r *Reconciler) status(ctx context.Context, object *v1.AgentDefinition, ready bool, reason, message string, revision *definition.Revision) error {
	base := object.DeepCopy()
	object.Status.ObservedGeneration = object.Generation
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, ObservedGeneration: object.Generation, Reason: reason, Message: message})
	if revision != nil {
		object.Status.CurrentRevision, object.Status.TemplateName = revision.ID, revision.Template.GetName()
	}
	if equality.Semantic.DeepEqual(base.Status, object.Status) {
		return nil
	}
	return r.Status().Patch(ctx, object, client.MergeFrom(base))
}

func (r *Reconciler) mapDependency(ctx context.Context, object client.Object) []reconcile.Request {
	definitions := &v1.AgentDefinitionList{}
	opts := []client.ListOption{}
	if object.GetNamespace() != "" {
		opts = append(opts, client.InNamespace(object.GetNamespace()))
	}
	if err := r.List(ctx, definitions, opts...); err != nil {
		ctrl.LoggerFrom(ctx).Error(diagnostics.SafeError(err), "Unable to map definition dependency")
		return nil
	}
	var requests []reconcile.Request
	for _, item := range definitions.Items {
		match := false
		if group, ok := object.(*v1.AgentGroup); ok {
			match = item.Spec.AgentGroupRef.Name == group.Name
		} else {
			match = item.Spec.TemplateRef != nil && item.Spec.TemplateRef.Name == object.GetName()
		}
		if match {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&item)})
		}
	}
	return requests
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client, r.Reader, r.Scheme = mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme()
	return ctrl.NewControllerManagedBy(mgr).For(&v1.AgentDefinition{}).
		Owns(sandbox.ExtensionObject("SandboxTemplate")).
		Watches(&v1.AgentGroup{}, handler.EnqueueRequestsFromMapFunc(r.mapDependency)).
		Watches(sandbox.ExtensionObject("SandboxTemplate"), handler.EnqueueRequestsFromMapFunc(r.mapDependency)).Complete(r)
}
