package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

// Publish only persisted condition transitions. Message-only changes and periodic
// observations do not create event traffic; configuration and Secret data are not used.
func RecordConditionChanges(recorder events.EventRecorder, object runtime.Object, before, after []metav1.Condition) {
	if recorder == nil {
		return
	}
	for _, condition := range after {
		old := meta.FindStatusCondition(before, condition.Type)
		if old != nil && old.Status == condition.Status && old.Reason == condition.Reason {
			continue
		}
		if condition.Reason == "" {
			continue
		}
		eventType := corev1.EventTypeNormal
		if condition.Status == metav1.ConditionFalse || condition.Reason == "PodFailed" {
			eventType = corev1.EventTypeWarning
		}
		switch condition.Reason {
		case ReasonRestarting, ReasonRestartCompleted,
			ReasonExpired, "ShutdownTimeReached", "FinishedTTLReached",
			ReasonPoolPending, ReasonAllocationSuspended, ReasonPoolAllocated, "PoolsDeleting",
			"NamespacePending", "QuotaPending", "PolicyDeleting", "Deleting",
			ReasonGroupNotReady, ReasonDependenciesNotReady,
			"SandboxSuspended", "Suspending", "PodTerminating", "PodNotTerminated", "PodSucceeded", "NotSuspended",
			"SchedulingGated", "PodStarting", "SandboxStarting", "ContainersNotReady", "PodNotReady", "SchedulingPending":
			eventType = corev1.EventTypeNormal
		}
		// Unknown readiness is ordinary waiting, rather than an actionable failure.
		if condition.Status == metav1.ConditionUnknown && condition.Type == ConditionReady {
			continue
		}
		recorder.Eventf(object, nil, eventType, condition.Reason, "Reconcile", "%s: %s", condition.Type, condition.Message)
	}
}
