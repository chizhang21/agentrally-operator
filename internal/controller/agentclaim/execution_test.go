package agentclaim

import (
	"context"
	"testing"

	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestAgentClaimExplicitRestartIsPersistedAndIdempotent(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	c, scheme := testutil.Client(t, nativeTemplate(w, source), group, ns, w)
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
	state := getAgentClaim(t, c, w)
	state.Spec.RestartToken = "restart-1"
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if deletes != 0 || getAgentClaim(t, c, w).Status.LastRestartToken != "restart-1" {
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
