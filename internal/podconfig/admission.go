package podconfig

import (
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Ignore admission-added sidecars, while keeping requested containers and init order exact.
func normalizeAdmissionContainers(left, right *corev1.PodSpec) bool {
	wanted := make([]corev1.Container, 0, len(left.Containers))
	for _, container := range left.Containers {
		index := AgentIndex(right.Containers, container.Name)
		if index < 0 {
			return false
		}
		observed := right.Containers[index].DeepCopy()
		if left.AutomountServiceAccountToken == nil || *left.AutomountServiceAccountToken {
			stripAdmissionTokenMounts(&container, observed, right.Volumes)
		}
		observed.SecurityContext = normalizeAdmissionContainerSecurity(container.SecurityContext, observed.SecurityContext)
		normalizeAdmissionResources(container.Resources, &observed.Resources)
		wanted = append(wanted, *observed)
	}
	right.Containers = wanted
	if len(left.InitContainers) != len(right.InitContainers) {
		return false
	}
	for i := range left.InitContainers {
		if left.InitContainers[i].Name != right.InitContainers[i].Name {
			return false
		}
		right.InitContainers[i].SecurityContext = normalizeAdmissionContainerSecurity(left.InitContainers[i].SecurityContext, right.InitContainers[i].SecurityContext)
		normalizeAdmissionResources(left.InitContainers[i].Resources, &right.InitContainers[i].Resources)
		if left.AutomountServiceAccountToken == nil || *left.AutomountServiceAccountToken {
			stripAdmissionTokenMounts(&left.InitContainers[i], &right.InitContainers[i], right.Volumes)
		}
	}
	return true
}

// LimitRange admission fills omitted requests and limits. Keep explicitly
// requested quantities exact, including missing keys, for every container.
func normalizeAdmissionResources(desired corev1.ResourceRequirements, actual *corev1.ResourceRequirements) {
	project := func(wanted, observed corev1.ResourceList) corev1.ResourceList {
		result := corev1.ResourceList{}
		for key := range wanted {
			if value, exists := observed[key]; exists {
				result[key] = value
			}
		}
		return result
	}
	actual.Requests = project(desired.Requests, actual.Requests)
	actual.Limits = project(desired.Limits, actual.Limits)
}

func normalizeAdmissionPlacement(left, right *corev1.PodSpec) {
	selectors := map[string]string{}
	for key := range left.NodeSelector {
		if value, exists := right.NodeSelector[key]; exists {
			selectors[key] = value
		}
	}
	right.NodeSelector = selectors
	tolerations := make([]corev1.Toleration, 0, len(left.Tolerations))
	for _, requested := range left.Tolerations {
		for _, observed := range right.Tolerations {
			if reflect.DeepEqual(requested, observed) {
				tolerations = append(tolerations, observed)
				break
			}
		}
	}
	right.Tolerations = tolerations
	if left.NodeName == "" {
		right.NodeName = ""
	}
	if left.Priority == nil {
		right.Priority = nil
	}
	if left.Overhead == nil {
		right.Overhead = nil
	}
	if left.Hostname == "" {
		right.Hostname = ""
	}
	if left.Subdomain == "" {
		right.Subdomain = ""
	}
}

func retainRequestedVolumes(left, right *corev1.PodSpec) bool {
	// Extra volumes belonging to injected sidecars do not alter Agent mounts.
	volumes := make([]corev1.Volume, 0, len(left.Volumes))
	for _, volume := range left.Volumes {
		found := false
		for _, existing := range right.Volumes {
			if existing.Name == volume.Name {
				volumes = append(volumes, existing)
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	right.Volumes = volumes
	return true
}

// SCC and Pod Security admission may fill omitted security fields. Explicit
// security requests remain exact; revision evidence detects later removals.
func normalizeAdmissionContainerSecurity(desired, actual *corev1.SecurityContext) *corev1.SecurityContext {
	if desired == nil {
		return nil
	}
	if actual != nil {
		omitUnrequestedSecurityFields(reflect.ValueOf(desired).Elem(), reflect.ValueOf(actual).Elem())
	}
	return actual
}

// omitUnrequestedSecurityFields projects observed security onto requested fields.
// Zero-valued fields are omitted; non-nil pointers to false or zero stay explicit.
// This admission-only rule does not relax declared-template or snapshot comparisons.
func omitUnrequestedSecurityFields(desired, actual reflect.Value) {
	for i := 0; i < desired.NumField(); i++ {
		wanted, observed := desired.Field(i), actual.Field(i)
		if wanted.IsZero() {
			observed.SetZero()
			continue
		}
		if wanted.Kind() == reflect.Pointer && wanted.Elem().Kind() == reflect.Struct && !observed.IsNil() {
			omitUnrequestedSecurityFields(wanted.Elem(), observed.Elem())
		}
	}
}

func stripAdmissionTokenMounts(desired, actual *corev1.Container, volumes []corev1.Volume) {
	const tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount"
	for _, mount := range desired.VolumeMounts {
		if mount.MountPath == tokenPath {
			return
		}
	}
	clean := make([]corev1.VolumeMount, 0, len(actual.VolumeMounts))
	for _, mount := range actual.VolumeMounts {
		injected := false
		if mount.MountPath == tokenPath && mount.ReadOnly && strings.HasPrefix(mount.Name, "kube-api-access-") {
			for _, volume := range volumes {
				if volume.Name != mount.Name || volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					if source.ServiceAccountToken != nil {
						injected = true
					}
				}
			}
		}
		if !injected {
			clean = append(clean, mount)
		}
	}
	actual.VolumeMounts = clean
}
