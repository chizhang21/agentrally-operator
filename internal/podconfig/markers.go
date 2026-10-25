package podconfig

import (
	"agentrally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
)

// AgentIndex resolves the named Agent container, defaulting to the first container.
func AgentIndex(containers []corev1.Container, name string) int {
	if name == "" && len(containers) > 0 {
		return 0
	}
	for i := range containers {
		if containers[i].Name == name {
			return i
		}
	}
	return -1
}

// Stamp records the revision on the selected Agent container without changing other env vars.
func Stamp(template *corev1.PodTemplateSpec, agentName, revision string) error {
	if template == nil || revision == "" {
		return diagnostics.New("InvalidConfiguration", "a Pod template and nonempty configuration revision are required", nil)
	}
	index := AgentIndex(template.Spec.Containers, agentName)
	if index < 0 {
		return diagnostics.New("InvalidConfiguration", "the selected Agent container does not exist", nil)
	}
	container := &template.Spec.Containers[index]
	clean := make([]corev1.EnvVar, 0, len(container.Env)+1)
	for _, env := range container.Env {
		if env.Name != RevisionEnv {
			clean = append(clean, env)
		}
	}
	container.Env = append(clean, corev1.EnvVar{Name: RevisionEnv, Value: revision})
	return nil
}

// StripMarkers removes configuration evidence before comparing execution inputs.
func StripMarkers(spec *corev1.PodSpec) {
	for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for i := range containers {
			clean := make([]corev1.EnvVar, 0, len(containers[i].Env))
			for _, env := range containers[i].Env {
				if env.Name != RevisionEnv {
					clean = append(clean, env)
				}
			}
			containers[i].Env = clean
		}
	}
}

// RevisionEnv records immutable configuration inputs on the Agent container.
const RevisionEnv = "AGENTRALLY_CONFIGURATION_REVISION"
