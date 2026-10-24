package agentinstance

import (
	"strings"
	"testing"
	"time"

	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

func TestExecutionDiagnosticsReachAgentInstance(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, reason, detail string
		status               corev1.PodStatus
	}{
		{"cpu", "Unschedulable", "insufficient CPU", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/3 nodes: 2 Insufficient cpu; do-not-expose"}}}},
		{"taint", "Unschedulable", "untolerated taints", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "node(s) had untolerated taint(s); do-not-expose"}}}},
		{"image", "ImagePullBackOff", "imagePullSecrets", corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "registry token do-not-expose"}}}}}},
		{"crash", "CrashLoopBackOff", "exit code 2", corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", RestartCount: 3, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "do-not-expose"}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Message: "do-not-expose"}}}}}},
		{"oom", "OOMKilled", "memory", corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}}},
		{"init-image", "ErrImagePull", "init-setup", corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{{Name: "init-setup", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}}}}},
		{"config", "CreateContainerConfigError", "Secret", corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: "do-not-expose"}}}}}},
		{"failed", "PodFailed", "Pod events", corev1.PodStatus{Phase: corev1.PodFailed, Message: "do-not-expose"}},
		{"completed", "PodSucceeded", "completed", corev1.PodStatus{Phase: corev1.PodSucceeded}},
		{"readiness", "ContainersNotReady", "probes", corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-time.Minute))}}}},
		{"readiness-gate", "PodNotReady", "readiness gates", corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}, {Type: corev1.PodReady, Status: corev1.ConditionFalse}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, ns, definition, w, source := testutil.Fixture()
			c, scheme := testutil.Client(t, definition, nativeTemplate(definition, source), g, ns, w)
			recorder := events.NewFakeRecorder(100)
			r := &Reconciler{Client: c, Scheme: scheme, Recorder: recorder, Now: func() time.Time { return now }}
			reconcileWithUpstreamSimulation(t, r, w)
			pod := ownedExecutionPod(t, c, w)
			pod.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
			if err := c.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			pod.Status = tc.status
			if err := c.Status().Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			for len(recorder.Events) > 0 {
				<-recorder.Events
			}
			reconcileWithUpstreamSimulation(t, r, w)
			state := getAgentInstance(t, c, w)
			ready := meta.FindStatusCondition(state.Status.Conditions, "Ready")
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != tc.reason || !strings.Contains(ready.Message, tc.detail) || ready.ObservedGeneration != state.Generation {
				t.Fatalf("expected %s containing %q, got %+v", tc.reason, tc.detail, ready)
			}
			if state.Status.PodName != pod.Name || state.Status.PodUID != string(pod.UID) {
				t.Fatal("execution Pod identity missing")
			}
			if scheduled := meta.FindStatusCondition(state.Status.Conditions, "PodScheduled"); scheduled != nil && strings.Contains(scheduled.Message, "do-not-expose") {
				t.Fatal("raw scheduler message leaked into status")
			}
			for len(recorder.Events) > 0 {
				if event := <-recorder.Events; strings.Contains(event, "do-not-expose") {
					t.Fatal("raw runtime message leaked into an Event")
				}
			}
			if strings.Contains(ready.Message, "do-not-expose") {
				t.Fatal("raw runtime message leaked into status")
			}
			reconcileWithUpstreamSimulation(t, r, w)
			if len(recorder.Events) != 0 {
				t.Fatal("unchanged failure emitted repeated Events")
			}
			// Recovery clears the failure, including a historical OOM termination.
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			pod.Status.InitContainerStatuses = nil
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}
			if err := c.Status().Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, w)
			testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "Ready", metav1.ConditionTrue)
		})
	}
}
