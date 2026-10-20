package agentclaim

import (
	"slices"

	v1 "arally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const dependencyIndex = "arally.io/dependency"

// Index only references present in the cached object; index callbacks never read
// the API or Secret values. Namespaces are supplied by the cache's index scope.
func claimDependencyKeys(object client.Object) []string {
	w := object.(*v1.AgentClaim)
	keys := []string{"AgentGroup/" + w.Spec.AgentGroupRef.Name}
	if w.Spec.TemplateRef != nil {
		keys = append(keys, "SandboxTemplate/"+w.Spec.TemplateRef.Name)
	}
	if workspace := w.Spec.Workspace; workspace != nil {
		if w.Spec.Template != "" && !workspace.ReadOnly {
			keys = append(keys, "Namespace/"+w.Namespace)
		}
	}
	for _, name := range w.Status.VolumeClaims {
		keys = append(keys, "PersistentVolumeClaim/"+name)
	}
	claimName := desiredPoolClaim(w).GetName()
	if state := w.Status.Provisioning; state != nil && state.ClaimName != "" {
		claimName = state.ClaimName
	}
	keys = append(keys, "SandboxClaim/"+claimName)
	if settings := w.Spec.Runtime; settings != nil {
		pod := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Env: settings.Env, EnvFrom: settings.EnvFrom}}}}
		keys = append(keys, podDependencyKeys(pod)...)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

func templateDependencyKeys(object client.Object) []string {
	template := object.(*unstructured.Unstructured)
	fields, found, err := unstructured.NestedMap(template.Object, "spec", "podTemplate")
	if err != nil || !found {
		return nil
	}
	pod := &corev1.PodTemplateSpec{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, pod); err != nil {
		return nil
	}
	return podDependencyKeys(pod)
}

func podDependencyKeys(pod *corev1.PodTemplateSpec) []string {
	var keys []string
	for _, ref := range configurationReferences(pod) {
		if ref.Kind == "ConfigMap" {
			keys = append(keys, "ConfigMap/"+ref.Name)
		}
	}
	for _, volume := range pod.Spec.Volumes {
		if ref := volume.PersistentVolumeClaim; ref != nil {
			keys = append(keys, "PersistentVolumeClaim/"+ref.ClaimName)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}
