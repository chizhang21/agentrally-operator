package agentinstance

import (
	"testing"
	"time"

	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExecutionSuspensionShowsTerminatingPodAndStall(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	pod := ownedExecutionPod(t, c, w)
	pod.Finalizers = []string{"test.example/hold-deletion"}
	if err := c.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	w = getAgentInstance(t, c, w)
	w.Spec.OperatingMode = sandbox.Suspended
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	assertReason(t, getAgentInstance(t, c, w), "Ready", "Suspending")
	// Only the test simulates Core deleting the Pod; Arally does not delete it.
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		delay  time.Duration
		reason string
	}{{time.Minute, "PodTerminating"}, {6 * time.Minute, "PodTerminationStalled"}} {
		r.Now = func() time.Time { return pod.DeletionTimestamp.Add(tc.delay) }
		reconcileWithUpstreamSimulation(t, r, w)
		state := getAgentInstance(t, c, w)
		assertReason(t, state, "Ready", tc.reason)
		if state.Status.PodName != pod.Name || state.Status.PodUID != string(pod.UID) || state.Status.AppliedRevision != "" {
			t.Fatal("terminating Pod must remain identifiable without claiming applied configuration")
		}
	}
	pod.Finalizers = nil
	if err := c.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	assertReason(t, getAgentInstance(t, c, w), "Ready", "Suspending")
	object := getSandbox(t, c, w)
	object.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Suspended", "status": "True", "reason": "PodTerminated", "observedGeneration": object.GetGeneration(), "lastTransitionTime": metav1.Now().Format(time.RFC3339)}}}
	if err := c.Status().Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentInstance(t, c, w)
	assertReason(t, state, "Ready", "SandboxSuspended")
	testutil.Condition(t, state.Status.Conditions, "Suspended", metav1.ConditionTrue)
	if state.Status.PodName != "" {
		t.Fatal("removed Pod still shown in status")
	}
}

func TestExecutionDiagnosticsPreserveConditionTransitionTimes(t *testing.T) {
	r, w, _ := interactionFixture(t, "OpenShiftRoute")
	reconcileWithUpstreamSimulation(t, r, w)
	object := getSandbox(t, r.Client, w)
	object.Object["status"] = map[string]interface{}{"selector": "agents.x-k8s.io/sandbox-name-hash=core-identity", "conditions": []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "PodReady", "observedGeneration": object.GetGeneration()},
		map[string]interface{}{"type": "PodScheduled", "status": "True", "reason": "PodScheduled", "observedGeneration": object.GetGeneration()},
	}}
	if err := r.Status().Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	route := readInteractionRoute(t, r, w, "OpenShiftRoute")
	if err := unstructured.SetNestedSlice(route.Object, []interface{}{map[string]interface{}{"host": "agent.example.com", "conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "True"}}}}, "status", "ingress"); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, route); err != nil {
		t.Fatal(err)
	}
	pod := ownedExecutionPod(t, r.Client, w)
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient cpu"}}}
	if err := r.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentInstance(t, r.Client, w)
	old := metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	for _, kind := range []string{"Ready", "PodScheduled", "InteractionReady"} {
		meta.FindStatusCondition(state.Status.Conditions, kind).LastTransitionTime = old
	}
	if err := r.Status().Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	version := state.ResourceVersion
	reconcileWithUpstreamSimulation(t, r, w)
	state = getAgentInstance(t, r.Client, w)
	if state.ResourceVersion != version {
		t.Fatal("unchanged execution failure rewrote status; condition transitions must be applied once")
	}
}

func TestExecutionStallThresholdAndCoreReadiness(t *testing.T) {
	now := time.Now().UTC()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-6 * time.Minute))}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if issue := podDiagnostic(pod, now); issue == nil || issue.Reason != "PodStartupStalled" {
		t.Fatalf("startup stall not diagnosed: %+v", issue)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: pod.CreationTimestamp}}
	if issue := podDiagnostic(pod, now); issue == nil || issue.Reason != "ReadinessStalled" {
		t.Fatalf("readiness stall not diagnosed: %+v", issue)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	_, _, _, w, _ := testutil.Fixture()
	r := &Reconciler{Now: func() time.Time { return now }}
	ready := metav1.Condition{Type: "Ready", Status: metav1.ConditionUnknown, Reason: "SandboxPending"}
	observed := r.executionReadiness(ctx, w, ready, pod, nil, false)
	if observed.Status != metav1.ConditionUnknown {
		t.Fatal("Pod readiness must not promote stale or missing Core readiness")
	}
	// Once Core confirms the generation, a healthy Pod clears prior diagnoses.
	ready.Status, ready.Reason = metav1.ConditionTrue, "PodReady"
	observed = r.executionReadiness(ctx, w, ready, pod, nil, false)
	if observed.Status != metav1.ConditionTrue || observed.Reason != "PodReady" {
		t.Fatal("healthy Core readiness changed")
	}
}

func TestMissingExecutionPodClearsStaleCoreReadiness(t *testing.T) {
	group, ns, definition, w, template := testutil.Fixture()
	kube, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, template), group, ns, w)
	r := &Reconciler{Client: kube, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	pod := ownedExecutionPod(t, kube, w)
	if err := kube.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err != nil {
		t.Fatal(err)
	}
	state := getAgentInstance(t, kube, w)
	ready := meta.FindStatusCondition(state.Status.Conditions, "Ready")
	if ready == nil || ready.Status == metav1.ConditionTrue || state.Status.PodName != "" {
		t.Fatalf("missing execution Pod must not remain ready: %+v", state.Status)
	}
}
