package agentinstance

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCoreAdmissionFeedbackIsSafeAndCurrent(t *testing.T) {
	for _, scenario := range []struct{ name, message, reason string }{
		{"pod-security", `Error seen: pods "agent" is forbidden: violates PodSecurity "restricted:latest": container "untrusted-upstream-detail" must not run as root`, "PodAdmissionDenied"},
		{"quota", `Error seen: pods "agent" is forbidden: exceeded quota: untrusted-upstream-detail`, "ReconcilerError"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			group, ns, definition, instance, source := testutil.Fixture()
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, instance)
			recorder := events.NewFakeRecorder(100)
			r := &Reconciler{Client: c, Scheme: scheme, Recorder: recorder}
			reconcileWithUpstreamSimulation(t, r, instance)
			object := getSandbox(t, c, instance)
			object.SetGeneration(1)
			if err := c.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			object.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
				"type": "Ready", "status": "False", "reason": "ReconcilerError", "message": scenario.message, "observedGeneration": int64(1),
			}}}
			if err := c.Status().Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			reconcile := func() *v1.AgentInstance {
				t.Helper()
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
					t.Fatal(err)
				}
				state := getAgentInstance(t, c, instance)
				status, err := json.Marshal(state.Status)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(status), "untrusted-upstream-detail") {
					t.Fatal("upstream error text leaked into Instance status")
				}
				for len(recorder.Events) > 0 {
					if strings.Contains(<-recorder.Events, "untrusted-upstream-detail") {
						t.Fatal("upstream error text leaked into an Operator Event")
					}
				}
				return state
			}
			state := reconcile()
			assertReason(t, state, "Available", scenario.reason)
			if meta.IsStatusConditionTrue(state.Status.Conditions, "Ready") {
				t.Fatal("a rejected execution became ready")
			}
			object = getSandbox(t, c, instance)
			object.SetGeneration(2)
			if err := c.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			assertReason(t, reconcile(), "Available", "SandboxPending")
			pod := ownedExecutionPod(t, c, instance)
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
			if err := c.Status().Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			nativeReady(t, c, getSandbox(t, c, instance))
			testutil.Condition(t, reconcile().Status.Conditions, "Ready", metav1.ConditionTrue)
		})
	}
}

func TestCoreLifecycleFeedbackRetainsFailedInstanceAndClearsStaleState(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	pod := ownedExecutionPod(t, c, w)
	pod.Status.Phase = corev1.PodFailed
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	object := getSandbox(t, c, w)
	object.SetGeneration(2)
	if err := c.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	object = getSandbox(t, c, w)
	current, err := sandbox.Decode(object)
	if err != nil {
		t.Fatal(err)
	}
	current.Status.Conditions = []metav1.Condition{{Type: "Finished", Status: metav1.ConditionTrue, Reason: "PodFailed", Message: "untrusted-upstream-detail", ObservedGeneration: 2, LastTransitionTime: metav1.Now()}, {Type: "Suspended", Status: metav1.ConditionTrue, Reason: "PodTerminated", ObservedGeneration: 2, LastTransitionTime: metav1.Now()}}
	encoded, err := sandbox.Encode(current)
	if err != nil {
		t.Fatal(err)
	}
	object.Object["status"] = encoded.Object["status"]
	if err := c.Status().Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	before := getSandbox(t, c, w)
	reconcileWithUpstreamSimulation(t, r, w)
	observed := getAgentInstance(t, c, w)
	testutil.Condition(t, observed.Status.Conditions, "Finished", metav1.ConditionTrue)
	if strings.Contains(meta.FindStatusCondition(observed.Status.Conditions, "Finished").Message, "untrusted-upstream-detail") {
		t.Fatal("raw task completion message leaked into Instance status")
	}
	if meta.FindStatusCondition(observed.Status.Conditions, "Suspended") != nil {
		t.Fatal("stale suspended state shown while running")
	}
	if getSandbox(t, c, w).GetResourceVersion() != before.GetResourceVersion() {
		t.Fatal("failed instance replaced or deleted")
	}
	// Mirror suspension only for the requested suspended mode.
	w = observed
	w.Spec.OperatingMode = sandbox.Suspended
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "Suspended", metav1.ConditionTrue)
	object = getSandbox(t, c, w)
	object.SetGeneration(3)
	if err := c.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	for _, kind := range []string{"Finished", "Suspended"} {
		if meta.FindStatusCondition(getAgentInstance(t, c, w).Status.Conditions, kind) != nil {
			t.Fatalf("stale %s retained", kind)
		}
	}
}

func TestReconcileEventsAreNotRepeated(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w)
	recorder := events.NewFakeRecorder(20)
	r := &Reconciler{Client: c, Scheme: scheme, Recorder: recorder}
	reconcileWithUpstreamSimulation(t, r, w)
	count := len(recorder.Events)
	if count == 0 {
		t.Fatal("no persisted condition events")
	}
	for len(recorder.Events) > 0 {
		<-recorder.Events
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if len(recorder.Events) != 0 {
		t.Fatal("unchanged reconcile emitted events")
	}
	w = getAgentInstance(t, c, w)
	w.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "missing"}, MountPath: "/workspace"}
	source.Spec.RuntimeClassName = ptr.To("missing")
	updateTemplate(t, c, w, source)
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if len(recorder.Events) == 0 {
		t.Fatal("preflight failure event missing")
	}
	// Events use the already-published condition, never the full PodTemplate.
	for len(recorder.Events) > 0 {
		if event := <-recorder.Events; strings.Contains(event, "USER_OPTION") {
			t.Fatal("template leaked into event")
		}
	}
}
