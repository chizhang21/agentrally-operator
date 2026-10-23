package agentinstance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/allocation"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/podconfig"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// desiredPoolClaim asks upstream to allocate an existing warm instance. In
// particular, it never sends Claim.env, which would force a cold allocation and
// cannot express Kubernetes SecretKeyRef values.
func desiredPoolClaim(agentClaim *v1.AgentInstance) *unstructured.Unstructured {
	if agentClaim == nil {
		return nil
	}
	nativeClaim := sandbox.ExtensionObject("SandboxClaim")
	sum := sha256.Sum256([]byte(agentClaim.Namespace + "/" + agentClaim.Name + "/" + string(agentClaim.UID)))
	suffix := "-claim-" + hex.EncodeToString(sum[:8])
	// Cold allocation uses this name for both Sandbox and headless Service.
	prefix := strings.ReplaceAll(agentClaim.Name, ".", "-")
	if prefix == "" || prefix[0] < 'a' || prefix[0] > 'z' {
		prefix = "agent-" + prefix
	}
	if len(prefix) > 63-len(suffix) {
		prefix = prefix[:63-len(suffix)]
	}
	prefix = strings.TrimRight(prefix, ".-")
	if prefix == "" {
		prefix = "agent"
	}
	nativeClaim.SetName(prefix + suffix)
	nativeClaim.SetNamespace(agentClaim.Namespace)
	nativeClaim.Object["spec"] = map[string]interface{}{"warmPoolRef": map[string]interface{}{"name": allocation.PoolName(agentClaim)}}
	return nativeClaim
}

// validatePoolTemplate compares the effective warm configuration before any
// allocation. Storage is part of the shared blueprint; Arally never rewrites a
// running warm Pod or injects per-Claim volume templates to fit a request.
func validatePoolTemplate(pool, template *unstructured.Unstructured, config allocation.Configuration) error {
	desired, service, claims := config.PodTemplate, config.Service, config.VolumeClaimTemplates
	if pool == nil || template == nil || desired == nil || pool.GetNamespace() == "" || pool.GetName() == "" || template.GetName() == "" {
		return poolDiagnostic("InvalidConfiguration", "a namespaced warm pool, SandboxTemplate and Pod template are required")
	}
	if pool.GroupVersionKind() != sandbox.ExtensionsGroupVersion.WithKind("SandboxWarmPool") || template.GroupVersionKind() != sandbox.ExtensionsGroupVersion.WithKind("SandboxTemplate") {
		return poolDiagnostic("PoolTemplateMismatch", "the warm pool or SandboxTemplate uses an unsupported API version or kind")
	}
	if pool.GetDeletionTimestamp() != nil || template.GetDeletionTimestamp() != nil {
		return poolDiagnostic("DependencyTerminating", "the warm pool or SandboxTemplate is terminating; choose another pool or wait for recreation")
	}
	ref, err := sandbox.PoolTemplateName(pool)
	if err != nil || ref == "" || ref != template.GetName() || pool.GetNamespace() != template.GetNamespace() {
		return poolDiagnostic("PoolTemplateMismatch", "the warm pool must reference this SandboxTemplate in the same namespace")
	}
	actualClaims, err := sandbox.TemplateVolumeClaims(template)
	if err != nil || !equality.Semantic.DeepEqual(actualClaims, claims) {
		return poolDiagnostic("PoolTemplateMismatch", "the warm volume claim templates differ from the requested storage configuration")
	}
	actualService, _, err := unstructured.NestedBool(template.Object, "spec", "service")
	if err != nil || actualService != service {
		return poolDiagnostic("PoolTemplateMismatch", "the warm template service setting differs from the requested workload")
	}
	actual, found, err := sandbox.TemplatePod(template)
	if err != nil || !found {
		return poolDiagnostic("PoolTemplateMismatch", "the warm SandboxTemplate has no valid native Pod template")
	}
	management, _, err := unstructured.NestedString(template.Object, "spec", "networkPolicyManagement")
	policy, _, policyErr := unstructured.NestedMap(template.Object, "spec", "networkPolicy")
	if err != nil || policyErr != nil {
		return poolDiagnostic("PoolTemplateMismatch", "the warm SandboxTemplate has invalid network policy settings")
	}
	if actual.Spec.DNSPolicy == "" && (management == "" || management == "Managed") && policy == nil {
		return poolDiagnostic("PoolTemplateMismatch", "Extensions would replace this template's DNS defaults; set networkPolicyManagement: Unmanaged or an explicit podTemplate.spec.dnsPolicy before allocation")
	}
	defaultWarmToken(actual)
	wanted := desired.DeepCopy()
	defaultWarmToken(wanted)
	sameShape := len(wanted.Spec.Containers) == len(actual.Spec.Containers) &&
		len(wanted.Spec.InitContainers) == len(actual.Spec.InitContainers) &&
		len(wanted.Spec.Volumes) == len(actual.Spec.Volumes)
	sameMetadata := reflect.DeepEqual(userPoolMetadata(wanted.Labels), userPoolMetadata(actual.Labels)) &&
		reflect.DeepEqual(userPoolMetadata(wanted.Annotations), userPoolMetadata(actual.Annotations))
	if !sameShape || !sameMetadata || !podconfig.MatchesDeclared(&wanted.Spec, &actual.Spec) {
		return poolDiagnostic("PoolTemplateMismatch", "the warm Pod template differs from the workload; use a matching pool and restart the allocation")
	}
	return nil
}

func validateAssignedSandbox(current *sandbox.Sandbox, config allocation.Configuration) error {
	desired, service, claims := config.PodTemplate, config.Service, config.VolumeClaimTemplates
	if current == nil || desired == nil {
		return poolDiagnostic("PoolTemplateMismatch", "the allocated Sandbox and desired Pod template must be available")
	}
	if current.Metadata.DeletionTimestamp != nil {
		return poolDiagnostic("DependencyTerminating", "the allocated Sandbox is terminating; wait for its replacement")
	}
	actual, wanted := current.Spec.PodTemplate.DeepCopy(), desired.DeepCopy()
	defaultWarmToken(actual)
	defaultWarmToken(wanted)
	if current.Spec.Service != service || !equality.Semantic.DeepEqual(current.Spec.VolumeClaimTemplates, claims) || !podconfig.MatchesDeclared(&wanted.Spec, &actual.Spec) ||
		!requestedPoolMetadata(wanted.Labels, actual.Labels) || !requestedPoolMetadata(wanted.Annotations, actual.Annotations) {
		return poolDiagnostic("PoolTemplateMismatch", "the allocated Sandbox differs from the workload; refresh the pool and restart the allocation")
	}
	return nil
}

func defaultWarmToken(template *corev1.PodTemplateSpec) {
	if template.Spec.AutomountServiceAccountToken == nil {
		value := false
		template.Spec.AutomountServiceAccountToken = &value
	}
}

func userPoolMetadata(values map[string]string) map[string]string {
	filtered := map[string]string{}
	for key, value := range values {
		if !strings.HasPrefix(key, "agents.x-k8s.io/") && !strings.HasPrefix(key, "extensions.agents.x-k8s.io/") {
			filtered[key] = value
		}
	}
	return filtered
}

func requestedPoolMetadata(wanted, actual map[string]string) bool {
	for key, value := range wanted {
		if observed, exists := actual[key]; !exists || observed != value {
			return false
		}
	}
	return true
}

func claimSandboxName(nativeClaim *unstructured.Unstructured) (string, error) {
	if nativeClaim == nil || nativeClaim.GroupVersionKind() != sandbox.ExtensionsGroupVersion.WithKind("SandboxClaim") {
		return "", poolDiagnostic("InvalidConfiguration", "a supported SandboxClaim is required")
	}
	name, err := sandbox.ClaimSandboxName(nativeClaim)
	if err != nil || (name != "" && len(validation.IsDNS1123Subdomain(name)) > 0) {
		return "", poolDiagnostic("PoolAllocationInvalid", "the SandboxClaim contains an invalid Sandbox reference")
	}
	return name, nil
}

func checkClaimSandboxOwner(object, nativeClaim *unstructured.Unstructured) error {
	if object == nil || nativeClaim == nil ||
		object.GroupVersionKind() != sandbox.GroupVersion.WithKind("Sandbox") ||
		nativeClaim.GroupVersionKind() != sandbox.ExtensionsGroupVersion.WithKind("SandboxClaim") ||
		nativeClaim.GetUID() == "" || nativeClaim.GetName() == "" || nativeClaim.GetNamespace() == "" ||
		object.GetName() == "" || object.GetNamespace() != nativeClaim.GetNamespace() {
		return poolDiagnostic("PoolOwnershipConflict", "the allocated Sandbox must be controlled by the current namespaced SandboxClaim")
	}
	owner := metav1.GetControllerOf(object)
	if owner == nil || owner.UID != nativeClaim.GetUID() || owner.Name != nativeClaim.GetName() || owner.Kind != "SandboxClaim" || owner.APIVersion != sandbox.ExtensionsGroupVersion.String() {
		return poolDiagnostic("PoolOwnershipConflict", fmt.Sprintf("Sandbox %s/%s is not controlled by the current SandboxClaim; refusing adoption", object.GetNamespace(), object.GetName()))
	}
	return nil
}

func poolDiagnostic(reason, message string) error {
	return diagnostics.New(reason, message, nil)
}

func allocationTimeout(w *v1.AgentInstance) int32 {
	if w.Spec.Provisioning != nil && w.Spec.Provisioning.ClaimTimeoutSeconds != nil {
		return *w.Spec.Provisioning.ClaimTimeoutSeconds
	}
	return 60
}
