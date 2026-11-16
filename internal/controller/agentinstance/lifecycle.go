package agentinstance

import (
	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// executionMode is explicit user intent. Health observations never change it.
func executionMode(instance *v1.AgentInstance) string {
	if instance.Spec.OperatingMode == "" {
		return sandbox.Running
	}
	return instance.Spec.OperatingMode
}

// Only the current owned terminal Pod can establish a completion. Core can
// briefly retain the old Pod's condition while suspension or resume progresses.
func executionFinishedCondition(w *v1.AgentInstance, pod *corev1.Pod, finished *metav1.Condition) *metav1.Condition {
	if executionMode(w) == sandbox.Suspended || pod == nil {
		return nil
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return nil
	}
	return finished
}

// Completion is diagnostic history, not a request to remove execution resources.
// Callers first verify Pod ownership and mirror current-generation Core conditions.
func executionCompletionStatus(w *v1.AgentInstance, pod *corev1.Pod) *v1.AgentInstanceLifecycleStatus {
	if pod != nil && pod.DeletionTimestamp == nil &&
		(pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning) {
		return nil
	}
	status := w.Status.Lifecycle.DeepCopy()
	finished := executionFinishedCondition(w, pod, meta.FindStatusCondition(w.Status.Conditions, controller.ConditionFinished))
	if finished == nil || finished.Status != metav1.ConditionTrue || finished.LastTransitionTime.IsZero() {
		return status
	}
	if status == nil || status.FinishedAt == nil {
		return &v1.AgentInstanceLifecycleStatus{FinishedAt: finished.LastTransitionTime.DeepCopy(), FinishedReason: finished.Reason}
	}
	return status
}
