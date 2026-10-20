package sandbox

import (
	"fmt"

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

// TemplateVolumeClaims reads the shared storage blueprint without resolving
// instance names. Claims stay on the template so allocation can use warm instances.
func TemplateVolumeClaims(template *unstructured.Unstructured) ([]corev1.PersistentVolumeClaimTemplate, error) {
	fields, _, err := unstructured.NestedSlice(template.Object, "spec", "volumeClaimTemplates")
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, nil
	}
	claims := make([]corev1.PersistentVolumeClaimTemplate, 0, len(fields))
	for _, field := range fields {
		value, ok := field.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("volumeClaimTemplates entries must be objects")
		}
		var claim corev1.PersistentVolumeClaimTemplate
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(value, &claim); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, nil
}

func SetTemplateVolumeClaims(template *unstructured.Unstructured, claims []corev1.PersistentVolumeClaimTemplate) error {
	if len(claims) == 0 {
		unstructured.RemoveNestedField(template.Object, "spec", "volumeClaimTemplates")
		return nil
	}
	fields := make([]interface{}, 0, len(claims))
	for i := range claims {
		value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&claims[i])
		if err != nil {
			return err
		}
		fields = append(fields, value)
	}
	return unstructured.SetNestedSlice(template.Object, fields, "spec", "volumeClaimTemplates")
}

// ExecutionPodTemplate projects Core's per-Sandbox PVC names for observation.
// The shared input remains unchanged and is used for revision and pool matching.
func ExecutionPodTemplate(name string, pod *corev1.PodTemplateSpec, claims []corev1.PersistentVolumeClaimTemplate) *corev1.PodTemplateSpec {
	result := pod.DeepCopy()
	names := VolumeClaimNames(name, claims)
	for _, claim := range claims {
		result.Spec.Volumes = append(result.Spec.Volumes, corev1.Volume{
			Name: claim.Name,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: names[claim.Name],
			}},
		})
	}
	return result
}

func VolumeClaimNames(name string, claims []corev1.PersistentVolumeClaimTemplate) map[string]string {
	if len(claims) == 0 {
		return nil
	}
	names := make(map[string]string, len(claims))
	for _, claim := range claims {
		names[claim.Name] = claim.Name + "-" + name
	}
	return names
}
