package agentclaim

import (
	"context"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Built-in writable workspaces use a supplemental volume group, independent of
// the image's UID. Native templates own their security settings; OpenShift's
// namespace SCC allocation takes precedence over the Kubernetes default.
func applyBuiltinWorkspacePermissions(ctx context.Context, reader client.Reader, w *v1.AgentClaim, template *corev1.PodTemplateSpec) error {
	if w.Spec.Template == "" || w.Spec.Workspace == nil || w.Spec.Workspace.ReadOnly {
		return nil
	}
	if template.Spec.SecurityContext != nil && template.Spec.SecurityContext.FSGroup != nil {
		return nil
	}
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: w.Namespace}, ns); err != nil {
		return diagnostics.ReferenceError("Namespace", "", w.Namespace, err)
	}
	// Restricted SCCs obtain the volume group from these namespace allocations.
	// Leave fsGroup unset so admission can select the allowed value.
	if ns.Annotations["openshift.io/sa.scc.supplemental-groups"] != "" || ns.Annotations["openshift.io/sa.scc.uid-range"] != "" {
		return nil
	}
	if template.Spec.SecurityContext == nil {
		template.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	template.Spec.SecurityContext.FSGroup = ptr.To(int64(1000))
	if template.Spec.SecurityContext.FSGroupChangePolicy == nil {
		template.Spec.SecurityContext.FSGroupChangePolicy = ptr.To(corev1.FSGroupChangeOnRootMismatch)
	}
	return nil
}
