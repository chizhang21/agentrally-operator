package agentinstance

import (
	"context"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCleanupStallIsVisibleDuringRestartExpiryAndDeletion(t *testing.T) {
	for _, operation := range []string{"restart", "expiry", "delete"} {
		t.Run(operation, func(t *testing.T) {
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
			switch operation {
			case "restart":
				state.Spec.RestartToken = "replace"
			case "expiry":
				state.Spec.Lifecycle = &v1.AgentInstanceLifecycle{ShutdownTime: &metav1.Time{Time: now.Add(-time.Second)}}
			case "delete":
				if err := c.Delete(ctx, state); err != nil {
					t.Fatal(err)
				}
			}
			if operation != "delete" {
				if err := c.Update(ctx, state); err != nil {
					t.Fatal(err)
				}
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
		})
	}
}

func TestAgentInstanceExplicitRestartIsPersistedAndIdempotent(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
	creates, deletes := 0, 0
	guarded := interceptor.NewClient(c, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if _, ok := o.(*corev1.PersistentVolumeClaim); ok {
				t.Fatal("Arally created a PVC")
			}
			if _, ok := o.(*corev1.Pod); ok {
				t.Fatal("Arally created Pod")
			}
			if o.GetObjectKind().GroupVersionKind().Kind == "SandboxClaim" {
				creates++
			}
			return c.Create(ctx, o, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			if _, ok := o.(*corev1.PersistentVolumeClaim); ok {
				t.Fatal("Arally deleted a PVC")
			}
			if _, ok := o.(*corev1.Pod); ok {
				t.Fatal("Arally deleted Pod")
			}
			if o.GetObjectKind().GroupVersionKind().Kind == "SandboxClaim" {
				deletes++
			}
			return c.Delete(ctx, o, opts...)
		},
	})
	r := &Reconciler{Client: guarded, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentInstance(t, c, w)
	state.Spec.RestartToken = "restart-1"
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if deletes != 0 || getAgentInstance(t, c, w).Status.LastRestartToken != "restart-1" {
		t.Fatal("restart request not persisted before deletion")
	}
	// Fresh reconciler instance simulates controller restart after accepting intent.
	r = &Reconciler{Client: guarded, Scheme: scheme}
	for i := 0; i < 5; i++ {
		reconcileWithUpstreamSimulation(t, r, w)
	}
	if creates != 2 || deletes != 1 {
		t.Fatalf("creates=%d deletes=%d", creates, deletes)
	}
}
