package agentinstance

import (
	"slices"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const dependencyIndex = "arally.io/dependency"

// Index only references present in the cached object; index callbacks never read
// the API or Secret values. Namespaces are supplied by the cache's index scope.
func instanceDependencyKeys(object client.Object) []string {
	w := object.(*v1.AgentInstance)
	keys := []string{"AgentGroup/" + w.Spec.AgentGroupRef.Name}
	keys = append(keys, "AgentDefinition/"+w.Spec.DefinitionRef.Name)
	if w.Status.Definition != nil {
		keys = append(keys, "SandboxTemplate/"+w.Status.Definition.TemplateName)
	}
	if workspace := w.Spec.Workspace; workspace != nil {
		if workspace.ExistingClaimRef != nil {
			keys = append(keys, "PersistentVolumeClaim/"+workspace.ExistingClaimRef.Name)
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
