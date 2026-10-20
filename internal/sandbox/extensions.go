package sandbox

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Every Arally AgentClaim requires Extensions templates, pools and allocation.
// Registering these wire types does not start informers.
var ExtensionsGroupVersion = schema.GroupVersion{Group: "extensions.agents.x-k8s.io", Version: "v1beta1"}

func ExtensionObject(kind string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(ExtensionsGroupVersion.WithKind(kind))
	return object
}

func AddExtensionsToScheme(scheme *runtime.Scheme) error {
	for _, kind := range []string{"SandboxClaim", "SandboxWarmPool", "SandboxTemplate"} {
		scheme.AddKnownTypeWithName(ExtensionsGroupVersion.WithKind(kind), ExtensionObject(kind))
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(ExtensionsGroupVersion.WithKind(kind + "List"))
		scheme.AddKnownTypeWithName(ExtensionsGroupVersion.WithKind(kind+"List"), list)
	}
	metav1.AddToGroupVersion(scheme, ExtensionsGroupVersion)
	return nil
}

// ExtensionList constructs an upstream list without depending on its controller implementation.
func ExtensionList(kind string) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ExtensionsGroupVersion.WithKind(kind + "List"))
	return list
}

// PoolTemplateName reads the upstream pool-to-template reference. Missing names
// remain empty so callers can apply their own validation or cleanup policy.
func PoolTemplateName(pool *unstructured.Unstructured) (string, error) {
	name, _, err := unstructured.NestedString(pool.Object, "spec", "sandboxTemplateRef", "name")
	return name, err
}

// ClaimPoolName reads the pool requested by an upstream SandboxClaim.
func ClaimPoolName(claim *unstructured.Unstructured) (string, error) {
	name, _, err := unstructured.NestedString(claim.Object, "spec", "warmPoolRef", "name")
	return name, err
}

// ClaimSandboxName reads the upstream allocation result. An empty name means the
// claim has not published an allocation yet; ownership is checked by the caller.
func ClaimSandboxName(claim *unstructured.Unstructured) (string, error) {
	name, _, err := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	return name, err
}

// TemplatePod decodes only the native Pod template portion of a SandboxTemplate.
// The presence flag distinguishes a missing template from an invalid Pod value.
func TemplatePod(template *unstructured.Unstructured) (*corev1.PodTemplateSpec, bool, error) {
	fields, found, err := unstructured.NestedMap(template.Object, "spec", "podTemplate")
	if err != nil || !found {
		return nil, found, err
	}
	pod := &corev1.PodTemplateSpec{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(fields, pod)
	return pod, true, err
}
