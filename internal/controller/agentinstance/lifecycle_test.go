package agentinstance

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestAgentInstanceLifecyclePolicy(t *testing.T) {
	anchor := metav1.NewTime(time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC))
	ttl, zero, negative := int32(60), int32(0), int32(-1)
	doNotRetain := false
	finished := func(reason string) *metav1.Condition {
		return &metav1.Condition{Type: "Finished", Status: metav1.ConditionTrue, Reason: reason, LastTransitionTime: anchor}
	}
	at := func(offset time.Duration) *metav1.Time { result := metav1.NewTime(anchor.Add(offset)); return &result }
	cases := []struct {
		name     string
		policy   *v1.AgentInstanceLifecycle
		prior    *v1.AgentInstanceLifecycleStatus
		finished *metav1.Condition
		now      time.Time
		expires  *metav1.Time
		expired  bool
		reason   string
		wantErr  bool
	}{
		{name: "no policy", now: anchor.Time},
		{name: "successful completion TTL", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &ttl}, finished: finished("PodSucceeded"), now: anchor.Add(10 * time.Second), expires: at(time.Minute), reason: "FinishedTTLReached"},
		{name: "deadline equal now", policy: &v1.AgentInstanceLifecycle{ShutdownTime: at(time.Minute)}, now: anchor.Add(time.Minute), expires: at(time.Minute), reason: "ShutdownTimeReached", expired: true},
		{name: "earlier absolute deadline", policy: &v1.AgentInstanceLifecycle{ShutdownTime: at(30 * time.Second), TTLSecondsAfterFinished: &ttl}, finished: finished("PodSucceeded"), now: anchor.Time, expires: at(30 * time.Second), reason: "ShutdownTimeReached"},
		{name: "earlier completion TTL", policy: &v1.AgentInstanceLifecycle{ShutdownTime: at(2 * time.Minute), TTLSecondsAfterFinished: &ttl}, finished: finished("PodSucceeded"), now: anchor.Time, expires: at(time.Minute), reason: "FinishedTTLReached"},
		{name: "equal deadlines keep absolute reason", policy: &v1.AgentInstanceLifecycle{ShutdownTime: at(time.Minute), TTLSecondsAfterFinished: &ttl}, finished: finished("PodSucceeded"), now: anchor.Time, expires: at(time.Minute), reason: "ShutdownTimeReached"},
		{name: "failed completion retained by default", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &zero}, finished: finished("PodFailed"), now: anchor.Add(time.Hour)},
		{name: "absolute deadline still ends failure", policy: &v1.AgentInstanceLifecycle{ShutdownTime: at(time.Minute), TTLSecondsAfterFinished: &zero}, finished: finished("PodFailed"), now: anchor.Add(time.Hour), expires: at(time.Minute), reason: "ShutdownTimeReached", expired: true},
		{name: "failed retention opt out", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &zero, RetainFailed: &doNotRetain}, finished: finished("PodFailed"), now: anchor.Time, expires: at(0), reason: "FinishedTTLReached", expired: true},
		{name: "zero TTL success", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &zero}, finished: finished("PodSucceeded"), now: anchor.Time, expires: at(0), reason: "FinishedTTLReached", expired: true},
		{name: "unknown transition is not invented", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &zero}, finished: &metav1.Condition{Type: "Finished", Status: metav1.ConditionTrue, Reason: "PodSucceeded"}, now: anchor.Time},
		{name: "finished false cannot start TTL", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &zero}, finished: &metav1.Condition{Type: "Finished", Status: metav1.ConditionFalse, LastTransitionTime: anchor}, now: anchor.Time},
		{name: "condition lost completion persists", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &ttl}, prior: &v1.AgentInstanceLifecycleStatus{FinishedAt: at(0), FinishedReason: "PodSucceeded"}, now: anchor.Add(30 * time.Second), expires: at(time.Minute), reason: "FinishedTTLReached"},
		{name: "failed reason survives condition loss", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &zero}, prior: &v1.AgentInstanceLifecycleStatus{FinishedAt: at(0), FinishedReason: "PodFailed"}, now: anchor.Add(time.Hour)},
		{name: "negative TTL rejected", policy: &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &negative}, now: anchor.Time, wantErr: true},
		{name: "unknown policy rejected", policy: &v1.AgentInstanceLifecycle{ShutdownPolicy: "Pause"}, now: anchor.Time, wantErr: true},
		{name: "delete shares deadline", policy: &v1.AgentInstanceLifecycle{ShutdownPolicy: "Delete", ShutdownTime: at(0)}, now: anchor.Time, expires: at(0), reason: "ShutdownTimeReached", expired: true},
		{name: "expiration survives removing policy", prior: &v1.AgentInstanceLifecycleStatus{ExpiredAt: at(0), ExpiresAt: at(0), Reason: "ShutdownTimeReached"}, now: anchor.Add(time.Hour), expires: at(0), reason: "ShutdownTimeReached", expired: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.prior.DeepCopy()
			decision, err := evaluateLifecycle(tc.policy, tc.prior, nil, tc.finished, tc.now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(tc.prior, before) {
				t.Fatal("input status mutated")
			}
			if tc.wantErr {
				return
			}
			if decision.Expired != tc.expired {
				t.Fatalf("decision = %#v, want expired=%v", decision, tc.expired)
			}
			var expires *metav1.Time
			var reason string
			if decision.Status != nil {
				expires, reason = decision.Status.ExpiresAt, decision.Status.Reason
			}
			if !reflect.DeepEqual(expires, tc.expires) || reason != tc.reason {
				t.Fatalf("expiration = %v/%s, want %v/%s", expires, reason, tc.expires, tc.reason)
			}
			if tc.expired && decision.Status.ExpiredAt == nil {
				t.Fatal("expiration was not latched")
			}
		})
	}
}

func TestAgentInstanceLifecycleCompletionDoesNotReset(t *testing.T) {
	now := time.Date(2026, 10, 9, 10, 0, 0, 456789, time.UTC)
	first := metav1.NewTime(now.Add(-time.Minute))
	prior := &v1.AgentInstanceLifecycleStatus{FinishedAt: &first, FinishedReason: "PodFailed"}
	newer := &metav1.Condition{Type: "Finished", Status: metav1.ConditionTrue, Reason: "PodSucceeded", LastTransitionTime: metav1.NewTime(now)}
	decision, err := evaluateLifecycle(nil, prior, nil, newer, now)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Status.FinishedAt.Equal(&first) || decision.Status.FinishedReason != "PodFailed" {
		t.Fatalf("completion reset: %#v", decision.Status)
	}
	deadline := metav1.NewTime(now.Add(-time.Second))
	decision, err = evaluateLifecycle(&v1.AgentInstanceLifecycle{ShutdownTime: &deadline}, prior, nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Expired || decision.Status.ExpiredAt.Nanosecond() != 0 || decision.Status.ExpiredAt.Location() != time.UTC {
		t.Fatalf("expiration precision: %#v", decision.Status)
	}
}

func TestAgentInstanceCompletionTTLDistinguishesSuspensionFromCompletion(t *testing.T) {
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		mode     string
		phase    corev1.PodPhase
		missing  bool
		blocked  bool
		deleting bool
		expired  bool
	}{
		{name: "suspending", mode: sandbox.Suspended, phase: corev1.PodSucceeded, deleting: true},
		{name: "resuming before Pod removal", mode: sandbox.Running, phase: corev1.PodSucceeded, deleting: true},
		{name: "resuming before replacement exists", mode: sandbox.Running, missing: true},
		{name: "resumed with stale Core completion", mode: sandbox.Running, phase: corev1.PodRunning},
		{name: "natural completion", mode: sandbox.Running, phase: corev1.PodSucceeded, expired: true},
		{name: "natural completion with broken configuration", mode: sandbox.Running, phase: corev1.PodSucceeded, blocked: true, expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group, ns, definition, w, source := testutil.Fixture()
			w.Spec.Lifecycle = &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: ptr.To(int32(0))}
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
			if expired := state.Status.Lifecycle != nil && state.Status.Lifecycle.ExpiredAt != nil; expired != tc.expired {
				t.Fatalf("completion TTL expired=%t, want %t; lifecycle=%+v", expired, tc.expired, state.Status.Lifecycle)
			}
			if !tc.expired && (state.Status.Lifecycle != nil && state.Status.Lifecycle.FinishedAt != nil || meta.FindStatusCondition(state.Status.Conditions, "Finished") != nil) {
				t.Fatal("suspension or a live Pod was recorded as completed")
			}
			reconcileWithUpstreamSimulation(t, r, w)
			if tc.expired {
				assertNoSandbox(t, c, w)
			} else if got := getSandbox(t, c, w); got.GetUID() != object.GetUID() {
				t.Fatal("suspension or resume replaced the Sandbox")
			}
		})
	}
}

func TestAgentInstanceLivePodInvalidatesStaleCompletionBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		absolute bool
		latched  bool
		readErr  bool
	}{
		{name: "stale completion TTL"},
		{name: "observation failure cannot expire a live Pod", readErr: true},
		{name: "absolute deadline still applies", absolute: true, readErr: true},
		{name: "latched expiry remains terminal", latched: true, readErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group, ns, definition, w, source := testutil.Fixture()
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
			r := &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
			reconcileWithUpstreamSimulation(t, r, w)
			pod := ownedExecutionPod(t, c, w)
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
			if err := c.Status().Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, w)
			w = getAgentInstance(t, c, w)
			finished := metav1.NewTime(now.Add(-time.Hour))
			w.Status.Lifecycle = &v1.AgentInstanceLifecycleStatus{FinishedAt: &finished, FinishedReason: "PodSucceeded"}
			if tc.latched {
				w.Status.Lifecycle.ExpiresAt, w.Status.Lifecycle.ExpiredAt = &finished, &finished
				w.Status.Lifecycle.Reason = "FinishedTTLReached"
			}
			if err := c.Status().Update(ctx, w); err != nil {
				t.Fatal(err)
			}
			w.Spec.Lifecycle = &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: ptr.To(int32(0))}
			if tc.absolute {
				w.Spec.Lifecycle.ShutdownTime = &finished
			}
			if err := c.Update(ctx, w); err != nil {
				t.Fatal(err)
			}
			if tc.readErr {
				r.Reader = interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, kube client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*corev1.Pod); ok {
						return apierrors.NewServiceUnavailable("Pod observation unavailable")
					}
					return kube.Get(ctx, key, object, opts...)
				}})
			}
			if tc.readErr && !tc.absolute && !tc.latched {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err == nil {
					t.Fatal("completion cleanup ignored failed Pod observation")
				}
				state := getAgentInstance(t, c, w)
				if state.Status.Lifecycle.ExpiredAt != nil {
					t.Fatal("unverified completion was marked expired")
				}
				getSandbox(t, c, w)
				return
			}
			for i := 0; i < 2; i++ {
				reconcileWithUpstreamSimulation(t, r, w)
			}
			state := getAgentInstance(t, c, w)
			if tc.absolute || tc.latched {
				testutil.Condition(t, state.Status.Conditions, "Expired", metav1.ConditionTrue)
				assertNoSandbox(t, c, w)
				return
			}
			if state.Status.Lifecycle != nil && (state.Status.Lifecycle.FinishedAt != nil || state.Status.Lifecycle.ExpiresAt != nil || state.Status.Lifecycle.ExpiredAt != nil) {
				t.Fatalf("stale completion survived resume: %+v", state.Status.Lifecycle)
			}
			getSandbox(t, c, w)
			testutil.Condition(t, state.Status.Conditions, "Available", metav1.ConditionTrue)
		})
	}
}

func TestAgentInstanceExpiryPrecedesBrokenDependencies(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentInstance(t, c, w)
	deadline := metav1.NewTime(time.Now().Add(-time.Hour))
	state.Spec.Lifecycle = &v1.AgentInstanceLifecycle{ShutdownTime: &deadline, ShutdownPolicy: "Retain"}
	state.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "missing"}, MountPath: "/workspace"}
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		reconcileWithUpstreamSimulation(t, r, w)
	}
	state = getAgentInstance(t, c, w)
	testutil.Condition(t, state.Status.Conditions, "Expired", metav1.ConditionTrue)
	assertNoSandbox(t, c, w)
	state.Spec.Lifecycle = nil
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	assertNoSandbox(t, c, w)
}

func TestAgentInstanceDeadlineRequeuesEvenWhenGroupMissing(t *testing.T) {
	_, _, definition, w, source := testutil.Fixture()
	now := time.Now().UTC()
	deadline := metav1.NewTime(now.Add(5 * time.Second))
	w.Spec.Lifecycle = &v1.AgentInstanceLifecycle{ShutdownTime: &deadline}
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), w)
	r := &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
	result := reconcileWithUpstreamSimulation(t, r, w)
	if result.RequeueAfter > 5*time.Second || result.RequeueAfter <= 0 {
		t.Fatalf("deadline lost on pending return: %v", result.RequeueAfter)
	}
}

func TestAgentInstanceCompletionTTLRetainsFailureThenCleansWhenAllowed(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	zero := int32(0)
	w.Spec.Lifecycle = &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: &zero}
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	pod := ownedExecutionPod(t, c, w)
	pod.Status.Phase = corev1.PodFailed
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	sb := getSandbox(t, c, w)
	sb.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Finished", "status": "True", "reason": "PodFailed", "observedGeneration": sb.GetGeneration(), "lastTransitionTime": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}}}
	if err := c.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		reconcileWithUpstreamSimulation(t, r, w)
	}
	state := getAgentInstance(t, c, w)
	getSandbox(t, c, w)
	if state.Status.Lifecycle.FinishedReason != "PodFailed" || state.Status.Lifecycle.ExpiredAt != nil {
		t.Fatal("failure was not retained")
	}
	// Confirmed completion survives the Pod disappearing before TTL cleanup.
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	state.Spec.Lifecycle.RetainFailed = ptr.To(false)
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		reconcileWithUpstreamSimulation(t, r, w)
	}
	state = getAgentInstance(t, c, w)
	assertNoSandbox(t, c, w)
	testutil.Condition(t, state.Status.Conditions, "Expired", metav1.ConditionTrue)
	if state.Status.Lifecycle.FinishedReason != "PodFailed" {
		t.Fatal("failure diagnostic lost during cleanup")
	}
}

func TestAgentInstanceExpiryDeleteUsesFinalizer(t *testing.T) {
	_, _, definition, w, source := testutil.Fixture()
	deadline := metav1.NewTime(time.Now().Add(-time.Hour))
	w.Spec.Lifecycle = &v1.AgentInstanceLifecycle{ShutdownTime: &deadline, ShutdownPolicy: "Delete"}
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), w)
	r := &Reconciler{Client: c, Scheme: scheme}
	for i := 0; i < 3; i++ {
		reconcileWithUpstreamSimulation(t, r, w)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), &v1.AgentInstance{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expired Workload not deleted: %v", err)
	}
}
