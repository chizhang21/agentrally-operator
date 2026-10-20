package agentgroup

import (
	"context"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *Reconciler) prepare(ctx context.Context, group *v1.AgentGroup) (bool, string, string, error) {
	level := "restricted"
	if group.Spec.PodSecurity == "Baseline" {
		level = "baseline"
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: group.NamespaceName()}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		if err := controller.PrepareOwned(ns, group, r.Scheme); err != nil {
			return err
		}
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		ns.Labels["pod-security.kubernetes.io/enforce"] = level
		return nil
	})
	if err != nil {
		return false, "", "", err
	}
	if ns.DeletionTimestamp != nil {
		return false, "NamespaceTerminating", "Waiting for the dedicated namespace to finish deletion", nil
	}
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "arally-quota", Namespace: ns.Name}}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "arally-group-policy", Namespace: ns.Name}}
	if group.Spec.Quota != nil {
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
			if err := controller.PrepareOwned(quota, group, r.Scheme); err != nil {
				return err
			}
			quota.Spec = *group.Spec.Quota.DeepCopy()
			return nil
		})
		if err != nil {
			return false, "", "", err
		}
	} else {
		done, err := controller.DeleteOwned(ctx, r.Client, quota, group)
		if err != nil {
			return false, "", "", err
		}
		if !done {
			return false, "PolicyDeleting", "Waiting for the removed quota to finish deletion", nil
		}
	}
	if group.Spec.NetworkPolicy != nil {
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
			if err := controller.PrepareOwned(policy, group, r.Scheme); err != nil {
				return err
			}
			policy.Spec = *group.Spec.NetworkPolicy.DeepCopy()
			return nil
		})
		if err != nil {
			return false, "", "", err
		}
	} else {
		done, err := controller.DeleteOwned(ctx, r.Client, policy, group)
		if err != nil {
			return false, "", "", err
		}
		if !done {
			return false, "PolicyDeleting", "Waiting for the removed network policy to finish deletion", nil
		}
	}
	if ns.Status.Phase != corev1.NamespaceActive {
		return false, "NamespacePending", "Waiting for the namespace to become Active", nil
	}
	if group.Spec.Quota != nil {
		for name, desired := range quota.Spec.Hard {
			observed, present := quota.Status.Hard[name]
			_, usagePresent := quota.Status.Used[name]
			if !present || !usagePresent || desired.Cmp(observed) != 0 {
				return false, "QuotaPending", "Waiting for Kubernetes quota admission state", nil
			}
		}
	}
	return true, "ResourcesPrepared", "Group namespace and policies are ready", nil
}
