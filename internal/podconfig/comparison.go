// Package podconfig compares execution inputs without changing its callers' objects.
// Declared templates stay strict, stamped snapshots retain creation evidence,
// and live Pods may contain admission additions. These are separate contracts.
package podconfig

import (
	"encoding/json"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// MatchesDeclared compares two declared Pod configurations, ignoring only revision
// markers and defaults. Extra containers, scheduling and security remain material.
func MatchesDeclared(desired, actual *corev1.PodSpec) bool {
	wanted, current := desired.DeepCopy(), actual.DeepCopy()
	StripMarkers(wanted)
	StripMarkers(current)
	normalizePodDefaults(wanted)
	normalizePodDefaults(current)
	return equality.Semantic.DeepEqual(wanted, current)
}

// MatchesSnapshot compares immutable template snapshots, including metadata and
// revision markers. Only semantic defaults are normalized.
func MatchesSnapshot(desired, actual *corev1.PodTemplateSpec) bool {
	left, right := desired.DeepCopy(), actual.DeepCopy()
	normalizePodDefaults(&left.Spec)
	normalizePodDefaults(&right.Spec)
	return equality.Semantic.DeepEqual(left, right)
}

// MatchesAdmitted compares requested configuration with a live Pod. Unrequested
// admission additions are projected away; requested fields and mounts stay exact.
func MatchesAdmitted(desired, actual *corev1.PodSpec) bool {
	left, right := desired.DeepCopy(), actual.DeepCopy()
	StripMarkers(left)
	StripMarkers(right)
	if !normalizeAdmissionContainers(left, right) || !retainRequestedVolumes(left, right) {
		return false
	}
	if left.SecurityContext == nil {
		right.SecurityContext = nil
	} else if right.SecurityContext != nil {
		omitUnrequestedSecurityFields(reflect.ValueOf(left.SecurityContext).Elem(), reflect.ValueOf(right.SecurityContext).Elem())
	}
	normalizeAdmissionPlacement(left, right)
	// ServiceAccount admission fills this list only when the submitted Pod has none.
	// Explicitly requested registry references remain part of drift detection.
	if len(left.ImagePullSecrets) == 0 {
		right.ImagePullSecrets = nil
	}
	normalizePodDefaults(left)
	normalizePodDefaults(right)
	// JSON treats nil and empty slices/maps identically wherever Kubernetes omits them.
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
