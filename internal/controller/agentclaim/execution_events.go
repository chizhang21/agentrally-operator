package agentclaim

import (
	"context"
	"time"

	"arally/operator/internal/controller"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Events add storage/runtime evidence absent from Pod conditions. Read only the
// verified Pod UID, bound the request, and never copy Event messages into status.
// Missing Event permissions must not block allocation or change execution state.
func (r *Reconciler) executionEventDiagnostic(ctx context.Context, pod *corev1.Pod) *executionDiagnostic {
	if pod == nil || pod.UID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	events := &corev1.EventList{}
	if err := r.referenceReader().List(ctx, events, client.InNamespace(pod.Namespace), client.MatchingFields{"involvedObject.uid": string(pod.UID)}, client.Limit(100)); err != nil || events.Continue != "" {
		return nil
	}
	return latestPodEventDiagnostic(pod, events.Items)
}

func latestPodEventDiagnostic(pod *corev1.Pod, events []corev1.Event) *executionDiagnostic {
	var latest *corev1.Event
	var latestTime time.Time
	for i := range events {
		event := &events[i]
		if event.InvolvedObject.UID != pod.UID || event.InvolvedObject.Kind != "Pod" || event.InvolvedObject.Name != pod.Name || event.Namespace != pod.Namespace {
			continue
		}
		observed := eventObservedAt(event)
		if observed.Before(pod.CreationTimestamp.Time) {
			continue
		}
		// Prefer a non-warning on ties: an old failure is not current evidence.
		if latest == nil || observed.After(latestTime) || (observed.Equal(latestTime) && event.Type != corev1.EventTypeWarning) {
			latest, latestTime = event, observed
		}
	}
	if latest == nil || latest.Type != corev1.EventTypeWarning {
		return nil
	}
	switch latest.Reason {
	case "FailedMount":
		return &executionDiagnostic{"VolumeMountFailed", "Kubelet reported a volume mount failure for this Pod; ask the cluster administrator to inspect PVC binding, CSI/node mount events and storage access"}
	case "FailedAttachVolume", "FailedMapVolume":
		return &executionDiagnostic{"VolumeAttachFailed", "A volume attach or map failure was reported for this Pod; inspect CSI, volume access modes and attachment to other nodes"}
	case "FailedCreatePodSandBox", "FailedCreatePodSandbox":
		return &executionDiagnostic{"PodSandboxCreationFailed", "Kubelet reported Pod sandbox creation failure; ask the cluster administrator to inspect RuntimeClass, node container runtime and CNI events"}
	}
	return nil
}

func eventObservedAt(event *corev1.Event) time.Time {
	latest := event.CreationTimestamp.Time
	for _, stamp := range []time.Time{event.EventTime.Time, event.LastTimestamp.Time} {
		if stamp.After(latest) {
			latest = stamp
		}
	}
	if event.Series != nil && event.Series.LastObservedTime.After(latest) {
		latest = event.Series.LastObservedTime.Time
	}
	return latest
}

func executionScheduledCondition(pod *corev1.Pod, generation int64) *metav1.Condition {
	if pod == nil || pod.DeletionTimestamp != nil {
		return nil
	}
	condition := podCondition(pod, corev1.PodScheduled)
	if condition == nil {
		return nil
	}
	result := &metav1.Condition{
		Type: controller.ConditionPodScheduled, Status: metav1.ConditionStatus(condition.Status), ObservedGeneration: generation,
		Reason: "PodScheduled", Message: "The execution Pod is assigned to a node",
	}
	if condition.Status == corev1.ConditionFalse {
		diagnostic := schedulingDiagnostic(condition)
		result.Reason, result.Message = diagnostic.Reason, diagnostic.Message
	} else if condition.Status != corev1.ConditionTrue {
		result.Status, result.Reason, result.Message = metav1.ConditionUnknown, "SchedulingPending", "Waiting for the scheduler to observe the execution Pod"
	}
	return result
}
