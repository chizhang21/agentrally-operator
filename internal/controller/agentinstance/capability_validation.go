package agentinstance

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/capabilities"
	"agentrally/operator/internal/controller"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Validation describes configuration evidence, never execution or enforcement.
type capabilityValidation struct {
	condition metav1.Condition
	messages  []string
}

func newCapabilityValidation(generation int64) capabilityValidation {
	return capabilityValidation{condition: metav1.Condition{
		Type: controller.ConditionCapabilityValidated, Status: metav1.ConditionTrue, ObservedGeneration: generation,
		Reason:  "ConfigurationObserved",
		Message: "Native references and available capability evidence checked; execution is not verified",
	}}
}

func (v *capabilityValidation) add(status metav1.ConditionStatus, reason, message string) {
	if status == metav1.ConditionTrue {
		return
	}
	if v.condition.Status == metav1.ConditionTrue || (status == metav1.ConditionFalse && v.condition.Status != metav1.ConditionFalse) {
		v.condition.Status, v.condition.Reason = status, reason
	}
	v.messages = append(v.messages, message)
	v.condition.Message = strings.Join(v.messages, "; ")
}

func validateCapabilities(ctx context.Context, reader client.Reader, w *v1.AgentInstance, group *v1.AgentGroup, pod *corev1.PodSpec, claims []corev1.PersistentVolumeClaimTemplate) metav1.Condition {
	v := newCapabilityValidation(w.Generation)
	validateExecutionPolicy(pod, group.Spec.ExecutionPolicy, &v)
	var report *v1.ClusterCapabilities
	if pod.RuntimeClassName != nil || len(pod.Volumes) > 0 || len(claims) > 0 || group.Spec.NetworkPolicy != nil {
		report = capabilities.ReadReport(ctx, reader)
	}
	if pod.RuntimeClassName != nil {
		validateRuntime(ctx, reader, pod, report, &v)
	}
	validateStorage(ctx, reader, w.Namespace, pod, report, &v)
	for _, claim := range claims {
		validateStorageClass(ctx, reader, claim.Name, claim.Spec.StorageClassName, report, &v)
	}
	if group.Spec.NetworkPolicy != nil {
		c := capabilities.NetworkCondition(report, w.Generation)
		v.add(c.Status, c.Reason, c.Message)
	}
	return v.condition
}

func validateRuntime(ctx context.Context, reader client.Reader, pod *corev1.PodSpec, report *v1.ClusterCapabilities, v *capabilityValidation) {
	rc := &nodev1.RuntimeClass{}
	if err := reader.Get(ctx, client.ObjectKey{Name: *pod.RuntimeClassName}, rc); err != nil {
		if apierrors.IsNotFound(err) {
			v.add(metav1.ConditionFalse, "RuntimeClassNotFound", fmt.Sprintf("RuntimeClass %q does not exist", *pod.RuntimeClassName))
		} else {
			v.add(metav1.ConditionUnknown, "RuntimeReadFailed", "RuntimeClass could not be read")
		}
		return
	}
	if rc.DeletionTimestamp != nil {
		v.add(metav1.ConditionFalse, "RuntimeClassTerminating", "Selected RuntimeClass is terminating")
		return
	}
	if !validateRuntimeSelector(pod, rc, v) {
		return
	}
	validateRuntimeEvidence(rc, report, v)
}

// RuntimeClass and Pod selectors must agree before admission can merge them.
// Node capacity, affinity and taints are evaluated by Kubernetes, not this check.
func validateRuntimeSelector(pod *corev1.PodSpec, rc *nodev1.RuntimeClass, v *capabilityValidation) bool {
	if rc.Scheduling == nil {
		return true
	}
	for key, value := range rc.Scheduling.NodeSelector {
		if supplied, ok := pod.NodeSelector[key]; ok && supplied != value {
			v.add(metav1.ConditionFalse, "RuntimeSchedulingConflict", fmt.Sprintf("RuntimeClass requires %s=%s but Pod nodeSelector requires %s", key, value, supplied))
			return false
		}
	}
	return true
}

func validateRuntimeEvidence(rc *nodev1.RuntimeClass, report *v1.ClusterCapabilities, v *capabilityValidation) {
	if !capabilities.SectionCurrent(report, "RuntimeDiscoveryReady", time.Now()) {
		v.add(metav1.ConditionUnknown, "RuntimeEvidenceUnavailable", "Runtime classification evidence is unavailable or stale")
		return
	}
	for _, observed := range report.Status.Runtimes {
		if observed.Name == rc.Name && observed.Handler == rc.Handler {
			// Counts in the report omit Pod-specific tolerations and affinity.
			if observed.Category == "Unknown" || observed.Category == "" {
				reason := observed.Reason
				if reason == "" {
					reason = "RuntimeIsolationUnknown"
				}
				v.add(metav1.ConditionUnknown, reason, "Selected handler's isolation classification is unknown; inspect ClusterCapabilities")
			}
			if observed.ImageContract == "Unknown" || observed.ImageContract == "" {
				v.add(metav1.ConditionUnknown, "RuntimeImageContractUnknown", "Selected handler's workload image contract is unknown")
			} else if observed.ImageContract != "LinuxOCI" {
				v.add(metav1.ConditionUnknown, "RuntimeImageCompatibilityUnverified", "Selected handler requires a specialized image contract; workload compatibility is not verified")
			}
			if evidence := observed.HandlerEvidence; evidence != nil && evidence.ReportedNodes == 0 && evidence.MissingFromReportedInventoryNodes > 0 {
				v.add(metav1.ConditionUnknown, "RuntimeHandlerNotReported", "Candidate node inventories do not report the selected handler; nodes without inventories remain unknown")
			} else if observed.Status == "Unknown" {
				v.add(metav1.ConditionUnknown, "RuntimeEvidenceUnknown", "Runtime configuration evidence is uncertain; inspect ClusterCapabilities")
			}
			return
		}
	}
	v.add(metav1.ConditionUnknown, "RuntimeEvidenceUnavailable", "Selected RuntimeClass is not in the current capability report")
}

func validateStorage(ctx context.Context, reader client.Reader, namespace string, pod *corev1.PodSpec, report *v1.ClusterCapabilities, v *capabilityValidation) {
	containers := append(append([]corev1.Container{}, pod.Containers...), pod.InitContainers...)
	for _, volume := range pod.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		name := volume.PersistentVolumeClaim.ClaimName
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				v.add(metav1.ConditionFalse, "PVCNotFound", fmt.Sprintf("PVC %q does not exist", name))
			} else {
				v.add(metav1.ConditionUnknown, "PVCReadFailed", fmt.Sprintf("PVC %q could not be read", name))
			}
			continue
		}
		if pvc.DeletionTimestamp != nil || pvc.Status.Phase == corev1.ClaimLost {
			v.add(metav1.ConditionFalse, "PVCUnavailable", fmt.Sprintf("PVC %q is terminating or lost", name))
			continue
		}
		block := pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock
		for _, container := range containers {
			for _, mount := range container.VolumeMounts {
				if mount.Name == volume.Name && block {
					v.add(metav1.ConditionFalse, "VolumeModeMismatch", fmt.Sprintf("PVC %q is raw Block and cannot be mounted as a directory", name))
				}
			}
			for _, device := range container.VolumeDevices {
				if device.Name == volume.Name && !block {
					v.add(metav1.ConditionFalse, "VolumeModeMismatch", fmt.Sprintf("PVC %q is Filesystem and cannot be exposed as a block device", name))
				}
			}
		}
		validateStorageClass(ctx, reader, name, pvc.Spec.StorageClassName, report, v)
		// Pending/WaitForFirstConsumer is allowed: Core must create the Pod first.
	}
}

func validateStorageClass(ctx context.Context, reader client.Reader, name string, storageClassName *string, report *v1.ClusterCapabilities, v *capabilityValidation) {
	if storageClassName == nil || *storageClassName == "" {
		v.add(metav1.ConditionUnknown, "StorageTypeUnrecognized", fmt.Sprintf("PVC %q has no StorageClass classification", name))
		return
	}
	sc := &storagev1.StorageClass{}
	if err := reader.Get(ctx, client.ObjectKey{Name: *storageClassName}, sc); err != nil {
		// Static PV matching can use a class name without a StorageClass object,
		// even before the claim binds. Missing classes are not conclusive failures.
		v.add(metav1.ConditionUnknown, "StorageEvidenceUnavailable", fmt.Sprintf("StorageClass for PVC %q could not be read; static or bound storage may remain usable", name))
		return
	}
	hasEvidence := false
	if capabilities.SectionCurrent(report, "StorageDiscoveryReady", time.Now()) {
		for _, observed := range report.Status.StorageClasses {
			if observed.Name == sc.Name && observed.Provisioner == sc.Provisioner && observed.Status == "Observed" {
				hasEvidence = true
				break
			}
		}
	}
	if !hasEvidence {
		v.add(metav1.ConditionUnknown, "StorageEvidenceUnavailable", fmt.Sprintf("Storage configuration evidence for PVC %q is incomplete or stale; volume usability is not determined", name))
	}
}
