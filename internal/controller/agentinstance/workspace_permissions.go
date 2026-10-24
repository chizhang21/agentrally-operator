package agentinstance

import (
	v1 "arally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// An explicit process UID also supplies the built-in writable volume group.
// Otherwise admission and storage configuration determine volume access.
func applyBuiltinWorkspacePermissions(w *v1.AgentInstance, builtin bool, template *corev1.PodTemplateSpec) {
	if !builtin || w.Spec.Workspace == nil || w.Spec.Workspace.ReadOnly {
		return
	}
	sc := template.Spec.SecurityContext
	if sc == nil || sc.FSGroup != nil || sc.RunAsUser == nil {
		return
	}
	sc.FSGroup = ptr.To(*sc.RunAsUser)
	if sc.FSGroupChangePolicy == nil {
		sc.FSGroupChangePolicy = ptr.To(corev1.FSGroupChangeOnRootMismatch)
	}
}
