package agentinstance

import (
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/podconfig"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type configurationObservation struct {
	Condition       metav1.Condition
	AppliedRevision string
}

// observeConfiguration reads immutable Pod spec, never mutable Pod metadata.
// A marker proves creation inputs; it does not attest application hot reloads.
func observeConfiguration(template *corev1.PodTemplateSpec, pod *corev1.Pod, agentName, desiredRevision string) configurationObservation {
	result := configurationObservation{Condition: metav1.Condition{
		Type: controller.ConditionConfigurationApplied, Status: metav1.ConditionUnknown,
		Reason: "PodNotObserved", Message: "the owned execution Pod has not been observed",
	}}
	if pod == nil {
		return result
	}
	if template == nil || podconfig.AgentIndex(template.Spec.Containers, agentName) < 0 {
		result.Condition.Reason = "ConfigurationUnverified"
		result.Condition.Message = "the desired Agent container is unavailable for configuration observation"
		return result
	}
	index := podconfig.AgentIndex(template.Spec.Containers, agentName)
	actualIndex := podconfig.AgentIndex(pod.Spec.Containers, template.Spec.Containers[index].Name)
	markerCount := 0
	if actualIndex >= 0 {
		for _, env := range pod.Spec.Containers[actualIndex].Env {
			if env.Name == podconfig.RevisionEnv {
				markerCount++
				if env.ValueFrom == nil {
					result.AppliedRevision = env.Value
				}
			}
		}
	}
	if !podconfig.MatchesAdmitted(&template.Spec, &pod.Spec) || markerCount > 1 || (markerCount == 1 && (result.AppliedRevision != desiredRevision || desiredRevision == "")) {
		result.Condition.Status = metav1.ConditionFalse
		result.Condition.Reason = "RestartRequired"
		result.Condition.Message = "the running Pod differs from the requested configuration; change spec.restartToken to replace the execution"
		return result
	}
	if markerCount == 0 || result.AppliedRevision == "" {
		result.Condition.Reason = "ConfigurationUnverified"
		result.Condition.Message = "native Pod configuration matches, but this Pod has no creation revision; referenced credential versions cannot be verified"
		return result
	}
	result.Condition.Status = metav1.ConditionTrue
	result.Condition.Reason = "ConfigurationCurrent"
	result.Condition.Message = "the owned Pod matches the requested configuration and its creation revision"
	return result
}
