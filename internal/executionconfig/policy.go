package executionconfig

import (
	"encoding/hex"
	"slices"
	"strings"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validateExecutionPolicy(pod *corev1.PodSpec, policy *v1.ExecutionPolicy, validation *capabilityValidation) {
	if policy == nil {
		return
	}
	if len(policy.AllowedRuntimeClasses) > 0 {
		if pod.RuntimeClassName == nil || !slices.Contains(policy.AllowedRuntimeClasses, *pod.RuntimeClassName) {
			validation.add(metav1.ConditionFalse, "RuntimeClassDenied", "Select an explicit RuntimeClass allowed by the AgentGroup execution policy")
		}
	}
	if policy.RequireImageDigest {
		for _, containers := range [][]corev1.Container{pod.Containers, pod.InitContainers} {
			for _, container := range containers {
				if !digestPinned(container.Image) {
					validation.add(metav1.ConditionFalse, "ImageDigestRequired", "All execution containers must use a supported image digest required by the AgentGroup")
					return
				}
			}
		}
	}
}

func digestPinned(image string) bool {
	name, digest, found := strings.Cut(image, "@")
	if !found || name == "" || strings.ContainsAny(name, " \t\r\n") {
		return false
	}
	algorithm, encoded, found := strings.Cut(digest, ":")
	length := map[string]int{"sha256": 64, "sha384": 96, "sha512": 128}[algorithm]
	if !found || length == 0 || len(encoded) != length || encoded != strings.ToLower(encoded) {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}
