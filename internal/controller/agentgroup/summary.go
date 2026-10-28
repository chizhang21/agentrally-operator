package agentgroup

import (
	"context"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *Reconciler) observe(ctx context.Context, group *v1.AgentGroup) (*v1.AgentGroupSummary, metav1.Condition, error) {
	capacity := metav1.Condition{Type: "CapacityReady", Status: metav1.ConditionTrue, ObservedGeneration: group.Generation, Reason: "NoManagedPools", Message: "No managed capacity is currently requested"}
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: group.NamespaceName()}, namespace); err != nil {
		return nil, capacity, client.IgnoreNotFound(err)
	}
	if controller.CheckGroupNamespace(namespace, group) != nil {
		return nil, capacity, nil
	}
	instances, pvcs, pools := &v1.AgentInstanceList{}, &corev1.PersistentVolumeClaimList{}, sandbox.ExtensionList("SandboxWarmPool")
	for _, list := range []client.ObjectList{instances, pvcs} {
		if err := r.List(ctx, list, client.InNamespace(namespace.Name)); err != nil {
			return nil, capacity, err
		}
	}
	// Pools are polled through the API reader without starting a dynamic informer.
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.List(ctx, pools, client.InNamespace(namespace.Name)); err != nil {
		pools.Items = nil
		capacity.Status, capacity.Reason, capacity.Message = metav1.ConditionUnknown, "CapacityObservationFailed", diagnostics.SafeError(err).Error()
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			capacity.Reason, capacity.Message = "AllocationAPIUnavailable", "Install agent-sandbox Extensions to manage capacity"
		}
	}
	matching := make([]v1.AgentInstance, 0, len(instances.Items))
	for _, instance := range instances.Items {
		if instance.Spec.AgentGroupRef.Name == group.Name {
			matching = append(matching, instance)
		}
	}
	result := &v1.AgentGroupSummary{Instances: controller.SummarizeInstances(matching), Storage: v1.AgentStorageSummary{
		PVCs: int32(len(pvcs.Items)), Requested: resource.MustParse("0"), Capacity: resource.MustParse("0"),
	}}
	for _, pvc := range pvcs.Items {
		result.Storage.Requested.Add(pvc.Spec.Resources.Requests[corev1.ResourceStorage])
		result.Storage.Capacity.Add(pvc.Status.Capacity[corev1.ResourceStorage])
	}
	for _, pool := range pools.Items {
		result.Pools.Total++
		desired, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
		result.Pools.DesiredSpareReplicas += int32(desired)
		observed, _, _ := unstructured.NestedInt64(pool.Object, "status", "observedGeneration")
		if pool.GetDeletionTimestamp() == nil && observed == pool.GetGeneration() {
			ready, _, _ := unstructured.NestedInt64(pool.Object, "status", "readyReplicas")
			result.Pools.ReadySpareReplicas += int32(ready)
		}
		if pool.GetLabels()[allocation.ManagedLabel] == "true" && controller.CheckOwner(&pool, group) == nil {
			if capacity.Status == metav1.ConditionTrue {
				capacity.Reason, capacity.Message = "CapacityPrepared", "Managed pool capacity is ready"
			}
			ready, _, _ := unstructured.NestedInt64(pool.Object, "status", "readyReplicas")
			if pool.GetDeletionTimestamp() != nil || observed != pool.GetGeneration() || ready < desired {
				capacity.Status, capacity.Reason, capacity.Message = metav1.ConditionFalse, "PoolRefilling", "Waiting for current-generation managed pool spare capacity"
			}
		}
	}
	return result, capacity, nil
}

func (r *Reconciler) mapGroupMember(_ context.Context, object client.Object) []reconcile.Request {
	var group string
	switch member := object.(type) {
	case *v1.AgentInstance:
		group = member.Spec.AgentGroupRef.Name
	case *v1.AgentDefinition:
		group = member.Spec.AgentGroupRef.Name
	}
	if group == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: group}}}
}
