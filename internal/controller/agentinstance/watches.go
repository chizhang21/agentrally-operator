package agentinstance

import (
	"context"
	"fmt"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Event handlers use cache indexes only. Slow validation and API reads belong in
// Reconcile, where the controller deadline and normal retries apply.
func (r *Reconciler) mapDependency(ctx context.Context, object client.Object) []reconcile.Request {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	namespace, key := object.GetNamespace(), ""
	throughTemplates := false
	switch dependency := object.(type) {
	case *v1.ClusterCapabilities:
		if dependency.Name != "cluster" {
			return nil
		}
		return r.dependencyRequests(ctx, "", "")
	case *v1.AgentGroup:
		namespace, key = dependency.NamespaceName(), "AgentGroup/"+dependency.Name
	case *v1.AgentDefinition:
		key = "AgentDefinition/" + dependency.Name
	case *unstructured.Unstructured:
		if dependency.GroupVersionKind() != sandbox.ExtensionsGroupVersion.WithKind("SandboxTemplate") {
			return nil
		}
		key = "SandboxTemplate/" + dependency.GetName()
	case *corev1.ConfigMap:
		key = "ConfigMap/" + dependency.Name
		throughTemplates = true
	case *corev1.PersistentVolumeClaim:
		key = "PersistentVolumeClaim/" + dependency.Name
		throughTemplates = true
	default:
		return nil
	}
	if namespace == "" {
		return nil
	}
	requests := r.dependencyRequests(ctx, namespace, key)
	if throughTemplates {
		return r.templateDependents(ctx, namespace, key, requests)
	}
	return requests
}

func (r *Reconciler) dependencyRequests(ctx context.Context, namespace, key string) []reconcile.Request {
	var options []client.ListOption
	if namespace != "" {
		options = append(options, client.InNamespace(namespace))
	}
	if key != "" {
		options = append(options, client.MatchingFields{dependencyIndex: key})
	}
	claims := &v1.AgentInstanceList{}
	if err := r.List(ctx, claims, options...); err != nil {
		ctrl.LoggerFrom(ctx).Error(diagnostics.SafeError(err), "Unable to map dependency to AgentInstances")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])})
	}
	return requests
}

func (r *Reconciler) templateDependents(ctx context.Context, namespace, key string, requests []reconcile.Request) []reconcile.Request {
	templates := sandbox.ExtensionList("SandboxTemplate")
	if err := r.List(ctx, templates, client.InNamespace(namespace), client.MatchingFields{dependencyIndex: key}); err != nil {
		ctrl.LoggerFrom(ctx).Error(diagnostics.SafeError(err), "Unable to map dependency to SandboxTemplates")
		return requests
	}
	seen := make(map[client.ObjectKey]bool, len(requests))
	for _, request := range requests {
		seen[request.NamespacedName] = true
	}
	for _, template := range templates.Items {
		for _, request := range r.dependencyRequests(ctx, namespace, "SandboxTemplate/"+template.GetName()) {
			if !seen[request.NamespacedName] {
				seen[request.NamespacedName] = true
				requests = append(requests, request)
			}
		}
	}
	return requests
}

// Core feedback follows the allocation Claim reference. Reconciliation verifies
// the full ownership and UID chain before using the Sandbox.
func (r *Reconciler) mapClaimSandbox(ctx context.Context, object client.Object) []reconcile.Request {
	owner := metav1.GetControllerOf(object)
	if owner == nil || owner.Kind != "SandboxClaim" || owner.APIVersion != sandbox.ExtensionsGroupVersion.String() || object.GetNamespace() == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.dependencyRequests(ctx, object.GetNamespace(), "SandboxClaim/"+owner.Name)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client, r.Scheme = mgr.GetClient(), mgr.GetScheme()
	r.Reader = mgr.GetAPIReader()
	r.Recorder = mgr.GetEventRecorder("arally-agentinstance")
	if r.Scheme == nil {
		return fmt.Errorf("manager scheme is required")
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1.AgentInstance{}, dependencyIndex, instanceDependencyKeys); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), sandbox.ExtensionObject("SandboxTemplate"), dependencyIndex, templateDependencyKeys); err != nil {
		return err
	}
	var err error
	r.InteractionClient, err = client.New(mgr.GetConfig(), client.Options{Scheme: r.Scheme, Mapper: mgr.GetRESTMapper()})
	if err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).For(&v1.AgentInstance{}).
		Watches(sandbox.Object(), handler.EnqueueRequestsFromMapFunc(r.mapClaimSandbox)).
		Owns(&corev1.Service{}).
		Owns(&coordinationv1.Lease{}).
		Watches(sandbox.ExtensionObject("SandboxTemplate"), handler.EnqueueRequestsFromMapFunc(r.mapDependency)).
		Watches(&v1.AgentDefinition{}, handler.EnqueueRequestsFromMapFunc(r.mapDependency)).
		Watches(&v1.AgentGroup{}, handler.EnqueueRequestsFromMapFunc(r.mapDependency)).
		Watches(&v1.ClusterCapabilities{}, handler.EnqueueRequestsFromMapFunc(r.mapDependency), builder.WithPredicates(controller.CapabilityChanges())).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.mapDependency)).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.mapDependency)).Complete(r)
}
