package agentinstance

import (
	"context"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func instanceTimings(w *v1.AgentInstance) *v1.AgentInstanceTimings {
	if w.Status.Timings == nil {
		w.Status.Timings = &v1.AgentInstanceTimings{}
	}
	return w.Status.Timings
}

func (r *Reconciler) suspensionRequest(ctx context.Context, w *v1.AgentInstance, hasExecution bool) error {
	base := w.DeepCopy()
	if executionMode(w) == sandbox.Suspended && hasExecution {
		timings := instanceTimings(w)
		if timings.SuspensionStartedAt == nil {
			now := metav1.NewTime(r.now())
			timings.SuspensionStartedAt = &now
		}
	} else if executionMode(w) != sandbox.Suspended && w.Status.Timings != nil {
		w.Status.Timings.SuspensionStartedAt, w.Status.Timings.SuspendedAt = nil, nil
	}
	return r.patchStatus(ctx, w, base)
}

func (r *Reconciler) observeTimings(w *v1.AgentInstance, previousPodUID string, pod *corev1.Pod) {
	if pod != nil {
		timings := instanceTimings(w)
		if previousPodUID != string(pod.UID) {
			timings.PodCreatedAt, timings.PodReadyAt = nil, nil
		}
		if timings.PodCreatedAt == nil && !pod.CreationTimestamp.IsZero() {
			timings.PodCreatedAt = pod.CreationTimestamp.DeepCopy()
		}
		if timings.PodReadyAt == nil && meta.IsStatusConditionTrue(w.Status.Conditions, controller.ConditionAvailable) {
			now := metav1.NewTime(r.now())
			timings.PodReadyAt = &now
		}
	}
	if executionMode(w) == sandbox.Suspended && meta.IsStatusConditionTrue(w.Status.Conditions, controller.ConditionSuspended) {
		timings := instanceTimings(w)
		if timings.SuspensionStartedAt != nil && timings.SuspendedAt == nil {
			now := metav1.NewTime(r.now())
			timings.SuspendedAt = &now
		}
	}
}
