package agentclaim

import (
	"context"
	"strings"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/templates"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resolvedTemplate is a snapshot of a built-in environment or a read-only native template.
type resolvedTemplate struct {
	PodTemplate *corev1.PodTemplateSpec
	Service     bool
}

func resolveSandboxTemplate(ctx context.Context, reader client.Reader, w *v1.AgentClaim) (*resolvedTemplate, error) {
	invalid := func(reason, message string) (*resolvedTemplate, error) {
		return nil, diagnostics.New(reason, message, nil)
	}
	if w == nil || w.Namespace == "" || (w.Spec.Template != "") == (w.Spec.TemplateRef != nil) {
		return invalid("InvalidConfiguration", "select exactly one of template or templateRef in a namespaced AgentClaim")
	}
	if w.Spec.Template != "" {
		pod, err := templates.Resolve(w.Spec.Template, w.Spec.Runtime, w.Spec.Workspace)
		return &resolvedTemplate{PodTemplate: pod, Service: true}, err
	}
	if w.Spec.Runtime != nil || len(validation.IsDNS1123Subdomain(w.Spec.TemplateRef.Name)) != 0 {
		return invalid("InvalidConfiguration", "templateRef.name must reference a SandboxTemplate in the AgentClaim namespace")
	}
	object := sandbox.ExtensionObject("SandboxTemplate")
	if err := reader.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Spec.TemplateRef.Name}, object); err != nil {
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return nil, diagnostics.New("TemplateAPIUnavailable", "Install agent-sandbox Core and Extensions before using AgentClaim templates", err)
		}
		return nil, diagnostics.ReferenceError("SandboxTemplate", w.Namespace, w.Spec.TemplateRef.Name, err)
	}
	if object.GetDeletionTimestamp() != nil {
		return invalid("DependencyTerminating", "The referenced SandboxTemplate is terminating; select an available template")
	}
	management, _, err := unstructured.NestedString(object.Object, "spec", "networkPolicyManagement")
	if err != nil || management != "Unmanaged" {
		return invalid("TemplatePolicyUnsupported", "Arally templates require networkPolicyManagement: Unmanaged; AgentGroup owns network policy")
	}
	if policy, found, err := unstructured.NestedMap(object.Object, "spec", "networkPolicy"); err != nil || (found && len(policy) > 0) {
		return invalid("TemplatePolicyUnsupported", "Configure network policy on AgentGroup; template networkPolicy is ignored by Unmanaged and cannot be used by Arally")
	}
	claims, _, err := unstructured.NestedSlice(object.Object, "spec", "volumeClaimTemplates")
	if err != nil || len(claims) > 0 {
		return invalid("TemplatePolicyUnsupported", "Arally templates use existing PVC references; volumeClaimTemplates would allocate PVCs outside Arally workspace ownership")
	}
	service, _, err := unstructured.NestedBool(object.Object, "spec", "service")
	if err != nil {
		return invalid("InvalidConfiguration", "SandboxTemplate.spec.service must be a boolean")
	}
	pod, found, err := sandbox.TemplatePod(object)
	if !found || pod == nil {
		return invalid("InvalidConfiguration", "SandboxTemplate requires a native podTemplate")
	}
	if err != nil || len(pod.Spec.Containers) == 0 {
		return invalid("InvalidConfiguration", "SandboxTemplate requires a valid native podTemplate with containers")
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "" || strings.TrimSpace(container.Image) == "" {
			return invalid("InvalidConfiguration", "SandboxTemplate containers require name and image")
		}
	}
	return &resolvedTemplate{PodTemplate: pod, Service: service}, nil
}
