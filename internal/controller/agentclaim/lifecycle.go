package agentclaim

import (
	"fmt"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type lifecycleDecision struct {
	Status  *v1.AgentClaimLifecycleStatus
	Expired bool
}

// Only the current owned terminal Pod can establish a completion. Core can
// briefly retain the old Pod's condition while suspension or resume progresses.
func executionFinishedCondition(w *v1.AgentClaim, pod *corev1.Pod, finished *metav1.Condition) *metav1.Condition {
	if w.Spec.OperatingMode == sandbox.Suspended || pod == nil {
		return nil
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return nil
	}
	return finished
}

func evaluateExecutionLifecycle(w *v1.AgentClaim, pod *corev1.Pod, now time.Time) (lifecycleDecision, error) {
	prior := w.Status.Lifecycle
	if prior != nil && prior.ExpiredAt == nil && pod != nil && pod.DeletionTimestamp == nil &&
		(pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning) {
		// A live owned Pod invalidates completion recorded for an earlier Pod.
		// Absolute deadlines are recomputed below; latched expiry stays terminal.
		prior = nil
	}
	finished := executionFinishedCondition(w, pod, meta.FindStatusCondition(w.Status.Conditions, "Finished"))
	return evaluateLifecycle(w.Spec.Lifecycle, prior, finished, now)
}

// retentionDeadline normalizes lifecycle timestamps to UTC second precision.
func retentionDeadline(anchor time.Time, retention time.Duration) time.Time {
	deadline := anchor.Add(retention)
	if deadline.IsZero() {
		return deadline
	}
	return deadline.Round(0).Truncate(time.Second).UTC()
}

// evaluateLifecycle plans cleanup without changing resources or input status.
// The caller must supply a Finished condition from the current execution.
func evaluateLifecycle(policy *v1.AgentClaimLifecycle, prior *v1.AgentClaimLifecycleStatus, finished *metav1.Condition, now time.Time) (lifecycleDecision, error) {
	status := prior.DeepCopy()
	if status == nil {
		status = &v1.AgentClaimLifecycleStatus{}
	}
	if status.ExpiredAt != nil {
		return lifecycleDecision{Status: status, Expired: true}, nil
	}
	if finished != nil && finished.Type == "Finished" && finished.Status == metav1.ConditionTrue && !finished.LastTransitionTime.IsZero() {
		if status.FinishedAt == nil {
			status.FinishedAt = finished.LastTransitionTime.DeepCopy()
			status.FinishedReason = finished.Reason
		} else if status.FinishedReason == "" && status.FinishedAt.Equal(&finished.LastTransitionTime) {
			status.FinishedReason = finished.Reason
		}
	}
	status.ExpiresAt, status.Reason = nil, ""
	if policy != nil {
		if policy.ShutdownPolicy != "" && policy.ShutdownPolicy != "Retain" && policy.ShutdownPolicy != "Delete" {
			return lifecycleDecision{}, fmt.Errorf("unsupported lifecycle shutdownPolicy %q", policy.ShutdownPolicy)
		}
		if policy.TTLSecondsAfterFinished != nil && *policy.TTLSecondsAfterFinished < 0 {
			return lifecycleDecision{}, fmt.Errorf("lifecycle ttlSecondsAfterFinished must be non-negative")
		}
		if policy.ShutdownTime != nil && !policy.ShutdownTime.IsZero() {
			deadline := metav1.NewTime(retentionDeadline(policy.ShutdownTime.Time, 0))
			status.ExpiresAt, status.Reason = &deadline, "ShutdownTimeReached"
		}
		retainFailed := policy.RetainFailed == nil || *policy.RetainFailed
		if policy.TTLSecondsAfterFinished != nil && status.FinishedAt != nil && !(retainFailed && status.FinishedReason == "PodFailed") {
			deadline := metav1.NewTime(retentionDeadline(status.FinishedAt.Time, time.Duration(*policy.TTLSecondsAfterFinished)*time.Second))
			if status.ExpiresAt == nil || deadline.Before(status.ExpiresAt) {
				status.ExpiresAt, status.Reason = &deadline, "FinishedTTLReached"
			}
		}
	}
	decision := lifecycleDecision{Status: status}
	if status.ExpiresAt != nil {
		if !now.Before(status.ExpiresAt.Time) {
			expiredAt := metav1.NewTime(retentionDeadline(now, 0))
			status.ExpiredAt, decision.Expired = &expiredAt, true
		}
	} else if prior == nil && status.FinishedAt == nil {
		decision.Status = nil
	}
	return decision, nil
}
