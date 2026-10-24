package agentinstance

import (
	"strings"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
)

func TestCoreLifecycleFeedbackRetainsFailedInstanceAndClearsStaleState(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	c, scheme := testutil.Client(t, definition, nativeTemplate(definition, source), g, ns, w)
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
	current.Status.Conditions = []metav1.Condition{{Type: "Finished", Status: metav1.ConditionTrue, Reason: "PodFailed", Message: "task failed", ObservedGeneration: 2, LastTransitionTime: metav1.Now()}, {Type: "Suspended", Status: metav1.ConditionTrue, Reason: "PodTerminated", ObservedGeneration: 2, LastTransitionTime: metav1.Now()}}
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
	c, scheme := testutil.Client(t, definition, nativeTemplate(definition, source), g, ns, w)
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
