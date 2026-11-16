package agentinstance

import (
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/testutil"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCleanupStallIsVisibleDuringDeletion(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
	now := time.Now().UTC().Truncate(time.Second)
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme, Now: func() time.Time { return now }}
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentInstance(t, c, w)
	claim := getClaim(t, c, w)
	claim.SetFinalizers([]string{"test/upstream-cleanup"})
	if err := c.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	state.Status.Timings = &v1.AgentInstanceTimings{CleanupStartedAt: &metav1.Time{Time: now.Add(-6 * time.Minute)}}
	if err := c.Status().Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, state); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err != nil {
			t.Fatal(err)
		}
	}
	state = getAgentInstance(t, c, w)
	assertReason(t, state, controller.ConditionCleanupReady, "ExecutionCleanupStalled")
	if len(state.Finalizers) == 0 || state.Status.Timings.CleanupCompletedAt != nil {
		t.Fatal("stalled cleanup was treated as complete")
	}
}
