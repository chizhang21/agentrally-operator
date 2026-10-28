package podconfig

import (
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// normalizePodDefaults applies Kubernetes admission defaults for semantic comparison.
func normalizePodDefaults(spec *corev1.PodSpec) {
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = corev1.RestartPolicyAlways
	}
	if spec.DNSPolicy == "" {
		spec.DNSPolicy = corev1.DNSClusterFirst
	}
	if spec.SchedulerName == "" {
		spec.SchedulerName = corev1.DefaultSchedulerName
	}
	if spec.ServiceAccountName == "" {
		spec.ServiceAccountName = "default"
	}
	spec.DeprecatedServiceAccount = ""
	if spec.TerminationGracePeriodSeconds == nil {
		value := int64(30)
		spec.TerminationGracePeriodSeconds = &value
	}
	if spec.EnableServiceLinks == nil {
		value := true
		spec.EnableServiceLinks = &value
	}
	if spec.AutomountServiceAccountToken == nil {
		value := true
		spec.AutomountServiceAccountToken = &value
	}
	if spec.ShareProcessNamespace != nil && !*spec.ShareProcessNamespace {
		spec.ShareProcessNamespace = nil
	}
	if spec.SecurityContext != nil && reflect.DeepEqual(*spec.SecurityContext, corev1.PodSecurityContext{}) {
		spec.SecurityContext = nil
	}
	if spec.PreemptionPolicy == nil {
		value := corev1.PreemptLowerPriority
		spec.PreemptionPolicy = &value
	}
	normalizeAdmissionTolerations(spec)
	for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for i := range containers {
			normalizeContainer(&containers[i])
		}
	}
	for i := range spec.Volumes {
		normalizeVolume(&spec.Volumes[i].VolumeSource)
	}
}

func normalizeAdmissionTolerations(spec *corev1.PodSpec) {
	tolerations := make([]corev1.Toleration, 0, len(spec.Tolerations))
	for _, toleration := range spec.Tolerations {
		unreachable := toleration.Key == "node.kubernetes.io/not-ready" || toleration.Key == "node.kubernetes.io/unreachable"
		defaultDuration := toleration.TolerationSeconds != nil && *toleration.TolerationSeconds == 300
		if unreachable && defaultDuration && toleration.Operator == corev1.TolerationOpExists && toleration.Effect == corev1.TaintEffectNoExecute {
			continue
		}
		tolerations = append(tolerations, toleration)
	}
	spec.Tolerations = tolerations
}

func normalizeVolume(source *corev1.VolumeSource) {
	if source.Secret != nil {
		normalizeOptionalReference(&source.Secret.Optional)
	}
	if source.ConfigMap != nil {
		normalizeOptionalReference(&source.ConfigMap.Optional)
	}
	if source.Projected != nil {
		for j := range source.Projected.Sources {
			projection := &source.Projected.Sources[j]
			if projection.Secret != nil {
				normalizeOptionalReference(&projection.Secret.Optional)
			}
			if projection.ConfigMap != nil {
				normalizeOptionalReference(&projection.ConfigMap.Optional)
			}
			if projection.ServiceAccountToken != nil && projection.ServiceAccountToken.ExpirationSeconds == nil {
				value := int64(3600)
				projection.ServiceAccountToken.ExpirationSeconds = &value
			}
			if projection.DownwardAPI != nil {
				normalizeDownwardAPI(projection.DownwardAPI.Items)
			}
		}
	}
	if source.Secret != nil && source.Secret.DefaultMode == nil {
		value := int32(420)
		source.Secret.DefaultMode = &value
	}
	if source.ConfigMap != nil && source.ConfigMap.DefaultMode == nil {
		value := int32(420)
		source.ConfigMap.DefaultMode = &value
	}
	if source.Projected != nil && source.Projected.DefaultMode == nil {
		value := int32(420)
		source.Projected.DefaultMode = &value
	}
	if source.DownwardAPI != nil && source.DownwardAPI.DefaultMode == nil {
		value := int32(420)
		source.DownwardAPI.DefaultMode = &value
	}
	if source.DownwardAPI != nil {
		normalizeDownwardAPI(source.DownwardAPI.Items)
	}
}

func normalizeDownwardAPI(items []corev1.DownwardAPIVolumeFile) {
	for i := range items {
		if ref := items[i].FieldRef; ref != nil && ref.APIVersion == "" {
			ref.APIVersion = "v1"
		}
	}
}

func normalizeContainer(container *corev1.Container) {
	if container.ImagePullPolicy == "" {
		container.ImagePullPolicy = corev1.PullIfNotPresent
		image := container.Image
		if !strings.Contains(image, "@") {
			leaf := image[strings.LastIndex(image, "/")+1:]
			if !strings.Contains(leaf, ":") || strings.HasSuffix(leaf, ":latest") {
				container.ImagePullPolicy = corev1.PullAlways
			}
		}
	}
	if container.TerminationMessagePath == "" {
		container.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	if container.SecurityContext != nil && reflect.DeepEqual(*container.SecurityContext, corev1.SecurityContext{}) {
		container.SecurityContext = nil
	}
	for i := range container.Ports {
		if container.Ports[i].Protocol == "" {
			container.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	for i := range container.Env {
		if source := container.Env[i].ValueFrom; source != nil {
			if source.SecretKeyRef != nil {
				normalizeOptionalReference(&source.SecretKeyRef.Optional)
			}
			if source.ConfigMapKeyRef != nil {
				normalizeOptionalReference(&source.ConfigMapKeyRef.Optional)
			}
		}
		if container.Env[i].ValueFrom != nil && container.Env[i].ValueFrom.FieldRef != nil && container.Env[i].ValueFrom.FieldRef.APIVersion == "" {
			container.Env[i].ValueFrom.FieldRef.APIVersion = "v1"
		}
	}
	for i := range container.EnvFrom {
		if source := container.EnvFrom[i].SecretRef; source != nil {
			normalizeOptionalReference(&source.Optional)
		}
		if source := container.EnvFrom[i].ConfigMapRef; source != nil {
			normalizeOptionalReference(&source.Optional)
		}
	}
	for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe, container.StartupProbe} {
		if probe == nil {
			continue
		}
		if probe.TimeoutSeconds == 0 {
			probe.TimeoutSeconds = 1
		}
		if probe.PeriodSeconds == 0 {
			probe.PeriodSeconds = 10
		}
		if probe.SuccessThreshold == 0 {
			probe.SuccessThreshold = 1
		}
		if probe.FailureThreshold == 0 {
			probe.FailureThreshold = 3
		}
		if probe.HTTPGet != nil && probe.HTTPGet.Scheme == "" {
			probe.HTTPGet.Scheme = corev1.URISchemeHTTP
		}
		if probe.HTTPGet != nil && probe.HTTPGet.Path == "" {
			probe.HTTPGet.Path = "/"
		}
	}
	for name, value := range container.Resources.Limits {
		if _, exists := container.Resources.Requests[name]; !exists {
			if container.Resources.Requests == nil {
				container.Resources.Requests = corev1.ResourceList{}
			}
			container.Resources.Requests[name] = value.DeepCopy()
		}
	}
}

// Kubernetes optional references default to required. Explicit true remains
// material; normalize false only on the deep copies used for observation.
func normalizeOptionalReference(optional **bool) {
	if *optional != nil && !**optional {
		*optional = nil
	}
}

// SetEnv replaces a native environment declaration without retaining duplicates.
func SetEnv(container *corev1.Container, desired corev1.EnvVar) {
	filtered := container.Env[:0]
	for _, env := range container.Env {
		if env.Name != desired.Name {
			filtered = append(filtered, env)
		}
	}
	container.Env = append(filtered, desired)
}
