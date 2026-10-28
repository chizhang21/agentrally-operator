package agentgroup

import (
	"context"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *Reconciler) invocationStatus(ctx context.Context, group *v1.AgentGroup, ready bool) error {
	if !controller.ManagedInvocation(group) || group.DeletionTimestamp != nil {
		meta.RemoveStatusCondition(&group.Status.Conditions, "InvocationAccessReady")
		meta.RemoveStatusCondition(&group.Status.Conditions, "IngressEnforcementVerified")
		return nil
	}
	condition := metav1.Condition{Type: "InvocationAccessReady", ObservedGeneration: group.Generation,
		Status: metav1.ConditionFalse, Reason: "GroupNotReady", Message: "Waiting for the Group policy"}
	if ready {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "PolicyConfigured", "Connector ingress is configured; traffic enforcement requires separate verification"
		reader := r.Reader
		if reader == nil {
			reader = r.Client
		}
		instances := &v1.AgentInstanceList{}
		if err := reader.List(ctx, instances, client.InNamespace(group.NamespaceName())); err != nil {
			return err
		}
		for _, instance := range instances.Items {
			if instance.Spec.Interaction != nil || instance.Status.Interaction != nil {
				condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "InteractionConflict", "Remove external interaction configuration before enabling managed invocation"
				break
			}
		}
		policies := &networkingv1.NetworkPolicyList{}
		if err := reader.List(ctx, policies, client.InNamespace(group.NamespaceName())); err != nil {
			return err
		}
		for _, policy := range policies.Items {
			if policy.Name != controller.GroupNetworkPolicyName && controller.IngressPolicy(&policy) {
				condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "PolicyConflict", "Additional ingress policies can bypass the Connector; managed invocation is blocked"
				break
			}
		}
	}
	meta.SetStatusCondition(&group.Status.Conditions, condition)
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{Type: "IngressEnforcementVerified", ObservedGeneration: group.Generation,
		Status: metav1.ConditionUnknown, Reason: "TrafficVerificationRequired", Message: "Policy configuration and CNI discovery do not prove traffic isolation"})
	return nil
}

func (r *Reconciler) mapNamespacePolicy(ctx context.Context, object client.Object) []reconcile.Request {
	if object.GetNamespace() == "" {
		return nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: object.GetNamespace()}, ns); err != nil {
		return nil
	}
	owner := metav1.GetControllerOf(ns)
	if owner == nil {
		groups := &v1.AgentGroupList{}
		if err := r.List(ctx, groups); err != nil {
			return nil
		}
		for _, group := range groups.Items {
			if group.Spec.PersistentWorkspaces && controller.CheckGroupNamespace(ns, &group) == nil {
				return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: group.Name}}}
			}
		}
		return nil
	}
	if owner.APIVersion != v1.GroupVersion.String() || owner.Kind != "AgentGroup" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: owner.Name}}}
}
