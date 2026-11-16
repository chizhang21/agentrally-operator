package agentinstance

import (
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAgentInstanceCompletionRequiresCurrentTerminalPod(t *testing.T) {
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		mode     string
		phase    corev1.PodPhase
		missing  bool
		blocked  bool
		deleting bool
		finished bool
	}{
		{name: "suspending", mode: sandbox.Suspended, phase: corev1.PodSucceeded, deleting: true},
		{name: "resuming before Pod removal", mode: sandbox.Running, phase: corev1.PodSucceeded, deleting: true},
		{name: "resuming before replacement exists", mode: sandbox.Running, missing: true},
		{name: "resumed with stale Core completion", mode: sandbox.Running, phase: corev1.PodRunning},
		{name: "natural completion", mode: sandbox.Running, phase: corev1.PodSucceeded, finished: true},
		{name: "natural completion with broken configuration", mode: sandbox.Running, phase: corev1.PodSucceeded, blocked: true, finished: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group, ns, definition, w, source := testutil.Fixture()
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
			r := &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
			reconcileWithUpstreamSimulation(t, r, w)
			pod := ownedExecutionPod(t, c, w)
			if tc.missing {
				if err := c.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}
			}
			if tc.deleting {
				pod.Finalizers = []string{"test.example/hold-deletion"}
				if err := c.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
				if err := c.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.missing {
				pod.Status.Phase = tc.phase
				if err := c.Status().Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			}
			w = getAgentInstance(t, c, w)
			w.Spec.OperatingMode, w.Generation = tc.mode, 2
			if tc.blocked {
				w.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "missing"}, MountPath: "/workspace"}
			}
			if err := c.Update(ctx, w); err != nil {
				t.Fatal(err)
			}
			if tc.phase == corev1.PodRunning {
				w.Status.Lifecycle = &v1.AgentInstanceLifecycleStatus{FinishedAt: &metav1.Time{Time: now.Add(-time.Hour)}, FinishedReason: "PodSucceeded"}
				if err := c.Status().Update(ctx, w); err != nil {
					t.Fatal(err)
				}
			}
			object := getSandbox(t, c, w)
			if err := unstructured.SetNestedField(object.Object, tc.mode, "spec", "operatingMode"); err != nil {
				t.Fatal(err)
			}
			object.SetGeneration(2)
			if err := c.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			object.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
				"type": "Finished", "status": "True", "reason": "PodSucceeded", "observedGeneration": int64(2),
				"lastTransitionTime": now.Add(-time.Minute).Format(time.RFC3339),
			}}}
			if err := c.Status().Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, w)
			if tc.blocked {
				reconcileWithUpstreamSimulation(t, r, w)
			}
			state := getAgentInstance(t, c, w)
			if finished := state.Status.Lifecycle != nil && state.Status.Lifecycle.FinishedAt != nil; finished != tc.finished {
				t.Fatalf("completion observed=%t, want %t; lifecycle=%+v", finished, tc.finished, state.Status.Lifecycle)
			}
			if !tc.finished && (state.Status.Lifecycle != nil && state.Status.Lifecycle.FinishedAt != nil || meta.FindStatusCondition(state.Status.Conditions, "Finished") != nil) {
				t.Fatal("suspension or a live Pod was recorded as completed")
			}
			r = &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now.Add(7 * 24 * time.Hour) }}
			reconcileWithUpstreamSimulation(t, r, w)
			if tc.finished {
				observed := getAgentInstance(t, c, w).Status.Lifecycle
				if observed == nil || !observed.FinishedAt.Equal(state.Status.Lifecycle.FinishedAt) || observed.FinishedReason != "PodSucceeded" {
					t.Fatal("completion diagnostics changed across reconciler restart")
				}
			}
			if got := getSandbox(t, c, w); got.GetUID() != object.GetUID() {
				t.Fatal("completion observation replaced the Sandbox")
			}
		})
	}
}
