package agentinstance

import (
	"context"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Explicit AnyUID intent never creates SCCs or RBAC grants. Namespace policy
// and ServiceAccount SCC authorization must be prepared by the administrator.
func applyAgentSecurity(ctx context.Context, reader client.Reader, namespace string, security *v1.AgentSecurity, template *corev1.PodTemplateSpec) error {
	if security == nil {
		return nil
	}
	profile := security.Profile
	if profile == "" {
		profile = "Restricted"
	}
	if profile != "Restricted" && profile != "AnyUID" {
		return diagnostics.New("InvalidConfiguration", "security.profile must be Restricted or AnyUID", nil)
	}
	if profile == "Restricted" && security.RunAsUser != nil && *security.RunAsUser == 0 {
		return diagnostics.New("InvalidConfiguration", "Restricted cannot run as UID 0", nil)
	}
	if security.RunAsUser != nil && *security.RunAsUser < 0 {
		return diagnostics.New("InvalidConfiguration", "security.runAsUser must be nonnegative", nil)
	}
	if profile == "AnyUID" {
		if security.ServiceAccountName == "" || security.ServiceAccountName == "default" {
			return diagnostics.New("SecurityAccountRequired", "AnyUID requires a dedicated serviceAccountName with administrator-granted SCC access", nil)
		}
		ns := &corev1.Namespace{}
		if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
			return diagnostics.ReferenceError("Namespace", "", namespace, err)
		}
		if ns.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
			return diagnostics.New("SecurityPolicyDenied", "AnyUID requires an administrator-managed AgentGroup with podSecurity: Baseline", nil)
		}
		if template.Annotations == nil {
			template.Annotations = map[string]string{}
		}
		template.Annotations["openshift.io/required-scc"] = "anyuid"
	}
	if security.ServiceAccountName != "" {
		account := &corev1.ServiceAccount{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: security.ServiceAccountName}, account); err != nil {
			return diagnostics.ReferenceError("ServiceAccount", namespace, security.ServiceAccountName, err)
		}
		if account.DeletionTimestamp != nil {
			return diagnostics.New("DependencyTerminating", "the selected ServiceAccount is terminating", nil)
		}
		template.Spec.ServiceAccountName = account.Name
	}
	if template.Spec.SecurityContext == nil {
		template.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	template.Spec.SecurityContext.RunAsNonRoot = ptr.To(profile == "Restricted")
	template.Spec.SecurityContext.RunAsUser = security.RunAsUser
	if profile == "AnyUID" {
		// The classic anyuid SCC may prohibit explicitly requested seccomp
		// profiles. Leave filtering to the cluster/runtime in this opt-in mode.
		template.Spec.SecurityContext.SeccompProfile = nil
	}
	for _, containers := range [][]corev1.Container{template.Spec.Containers, template.Spec.InitContainers} {
		for i := range containers {
			sc := containers[i].SecurityContext
			if sc == nil {
				sc = &corev1.SecurityContext{}
				containers[i].SecurityContext = sc
			}
			sc.RunAsNonRoot = ptr.To(profile == "Restricted")
			sc.RunAsUser = security.RunAsUser
			if profile == "AnyUID" {
				sc.SeccompProfile = nil
			}
		}
	}
	return nil
}
