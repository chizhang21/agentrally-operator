package agentinstance

import (
	"context"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This is a diagnostic threshold, not a deadline for killing or replacing Pods.
const executionStallThreshold = diagnostics.ExecutionStallThreshold

type executionDiagnostic = diagnostics.ExecutionDiagnostic

// Core remains the authority for successful execution. Direct, UID-verified Pod
// observations can explain a failure before Core has refreshed its conditions.
func (r *Reconciler) executionReadiness(ctx context.Context, w *v1.AgentInstance, ready metav1.Condition, pod *corev1.Pod, podErr error, suspended bool) metav1.Condition {
	var diagnostic *executionDiagnostic
	switch {
	case podErr != nil:
		ready.Status = metav1.ConditionUnknown
		ready.Reason = "PodObservationBlocked"
		ready.Message = "Cannot inspect the execution Pod; check Sandbox ownership and operator Pod read permissions"
		ready.LastTransitionTime = metav1.Time{}
		return ready
	case executionMode(w) == sandbox.Suspended:
		diagnostic = diagnostics.Suspension(pod, r.now())
		if pod == nil && !suspended {
			// Core must still acknowledge the current Sandbox generation.
			diagnostic = &executionDiagnostic{Reason: "Suspending", Message: "Waiting for Core to confirm suspension after the execution Pod is removed"}
		}
	case pod == nil:
		if ready.Status == metav1.ConditionTrue {
			diagnostic = &executionDiagnostic{Reason: "PodNotObserved", Message: "The execution Pod does not exist; waiting for Core to create it and refresh readiness"}
		}
	default:
		diagnostic = diagnostics.Pod(pod, r.now())
		if diagnostic != nil && (diagnostic.Reason == "PodStarting" || diagnostic.Reason == "SandboxStarting" || diagnostic.Reason == "PodStartupStalled") {
			if event := r.executionEventDiagnostic(ctx, pod); event != nil {
				diagnostic = event
			}
		}
	}
	if diagnostic != nil {
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, diagnostic.Reason, diagnostic.Message
		// A Pod diagnosis did not occur at Core's earlier readiness transition.
		// SetStatusCondition records a new transition only when status changes.
		ready.LastTransitionTime = metav1.Time{}
	}
	return ready
}

func podCondition(pod *corev1.Pod, kind corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == kind {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}
