package agentgroup

import (
	"context"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *Reconciler) summary(ctx context.Context, group *v1.AgentGroup) (*v1.AgentGroupSummary, error) {
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: group.NamespaceName()}, namespace); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if controller.CheckOwner(namespace, group) != nil {
		return nil, nil
	}
	instances, pvcs, pools := &v1.AgentInstanceList{}, &corev1.PersistentVolumeClaimList{}, sandbox.ExtensionList("SandboxWarmPool")
	for _, list := range []client.ObjectList{instances, pvcs, pools} {
		var reader client.Reader = r.Client
		if list == pools && r.Reader != nil {
			// Pools are polled, not watched. Do not start a dynamic informer that
			// needs watch permission or can retain stale refill counts.
			reader = r.Reader
		}
		if err := reader.List(ctx, list, client.InNamespace(namespace.Name)); err != nil {
			// Group policy can still report readiness when Extensions is not installed.
			if list == pools && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err)) {
				continue
			}
			return nil, err
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
	}
	return result, nil
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
