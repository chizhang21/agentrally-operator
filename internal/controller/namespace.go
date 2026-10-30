package controller

import (
	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CheckGroupNamespace verifies either Group ownership or a pinned persistent namespace.
func CheckGroupNamespace(namespace client.Object, group *v1.AgentGroup) error {
	if !group.Spec.PersistentWorkspaces {
		return CheckOwner(namespace, group)
	}
	if namespace.GetName() != group.NamespaceName() || group.Status.NamespaceUID == "" || namespace.GetUID() != group.Status.NamespaceUID || len(namespace.GetOwnerReferences()) != 0 {
		return diagnostics.New("NamespaceBindingIncomplete", "Persistent namespace identity is not established", nil)
	}
	return nil
}

func GroupPolicyOwner(group *v1.AgentGroup) client.Object {
	if group.Spec.PersistentWorkspaces {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: group.NamespaceName(), UID: group.Status.NamespaceUID}}
	}
	return group
}
