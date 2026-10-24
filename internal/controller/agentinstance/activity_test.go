package agentinstance

import (
	"encoding/json"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestActivityStatusSurvivesAPIRoundTrip(t *testing.T) {
	_, _, _, w, _ := testutil.Fixture()
	now := time.Date(2026, 10, 24, 10, 0, 0, 123456000, time.UTC)
	w.Spec.Lifecycle = &v1.AgentInstanceLifecycle{IdleTimeoutSeconds: ptr.To(int32(60)), ActivityLeaseRef: &v1.ActivityLeaseReference{Name: "activity"}}
	w.Status.SandboxUID = "execution"
	w.Status.Provisioning = &v1.AgentInstanceProvisioningStatus{CompletedAt: &metav1.Time{Time: now.Add(-time.Hour)}}
	activity, issue := activityObservation(w, activityLease(t, w, now), now)
	if issue != "" {
		t.Fatal(issue)
	}
	encoded, err := json.Marshal(activity)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &v1.AgentInstanceActivityStatus{}
	if err := json.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(activity, decoded) {
		t.Fatal("activity timestamps change on every API round trip")
	}
}

func activityLease(t *testing.T, w *v1.AgentInstance, now time.Time) *coordinationv1.Lease {
	t.Helper()
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "activity", Namespace: w.Namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "AgentInstance", Name: w.Name, UID: w.UID, Controller: ptr.To(true)}},
		Annotations:     map[string]string{activityStateAnnotation: "Idle", lastActivityAnnotation: now.Add(-time.Minute).Format(time.RFC3339Nano)}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(string(w.Status.SandboxUID)), RenewTime: &metav1.MicroTime{Time: now}, LeaseDurationSeconds: ptr.To(int32(120))}}
	return lease
}

func TestActivityRejectsUntrustedAndStaleSignals(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"missing", "foreign-owner", "old-execution", "stale", "future", "future-activity", "regressed", "invalid-state", "busy"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, _, w, _ := testutil.Fixture()
			w.Spec.Lifecycle = &v1.AgentInstanceLifecycle{IdleTimeoutSeconds: ptr.To(int32(10)), ActivityLeaseRef: &v1.ActivityLeaseReference{Name: "activity"}}
			w.Status.SandboxUID = "current-execution"
			w.Status.Provisioning = &v1.AgentInstanceProvisioningStatus{CompletedAt: &metav1.Time{Time: now.Add(-time.Hour)}}
			lease := activityLease(t, w, now)
			switch scenario {
			case "foreign-owner":
				lease.OwnerReferences = nil
			case "old-execution":
				lease.Spec.HolderIdentity = ptr.To("old-execution")
			case "stale":
				lease.Spec.RenewTime.Time = now.Add(-time.Hour)
			case "future":
				lease.Spec.RenewTime.Time = now.Add(time.Minute)
			case "future-activity":
				lease.Annotations[lastActivityAnnotation] = now.Add(time.Minute).Format(time.RFC3339)
			case "regressed":
				w.Status.Activity = &v1.AgentInstanceActivityStatus{LastActivityAt: &metav1.MicroTime{Time: now.Add(-time.Second)}}
			case "invalid-state":
				lease.Annotations[activityStateAnnotation] = "Healthy"
			case "busy":
				lease.Annotations[activityStateAnnotation] = "Busy"
			}
			objects := []client.Object{w}
			if scenario != "missing" {
				objects = append(objects, lease)
			}
			c, scheme := testutil.Client(t, objects...)
			r := &Reconciler{Client: c, Reader: c, Scheme: scheme, Now: func() time.Time { return now }}
			if err := r.observeActivity(ctx, w); err != nil {
				t.Fatal(err)
			}
			state := getAgentInstance(t, c, w)
			if state.Status.Activity != nil && state.Status.Activity.Suspended {
				t.Fatal("untrusted or busy signal suspended execution")
			}
			if meta.IsStatusConditionTrue(state.Status.Conditions, "ActivityReady") != (scenario == "busy") {
				t.Fatal("unexpected signal acceptance", state.Status.Conditions)
			}
		})
	}
}

func TestIdleWakePreservesRevisionAndHonorsManualSuspensionAndMaximumLifetime(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	w.Spec.Lifecycle = &v1.AgentInstanceLifecycle{IdleTimeoutSeconds: ptr.To(int32(60)), ActivityLeaseRef: &v1.ActivityLeaseReference{Name: "activity"}}
	c, scheme := clientWithPublishedDefinitions(t, group, ns, definition, w, nativeTemplate(definition, source))
	now := time.Now().UTC().Truncate(time.Second)
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme, Now: func() time.Time { return now }}
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentInstance(t, c, w)
	revision := state.Status.Definition.Revision
	lease := activityLease(t, state, now)
	lease.Annotations[lastActivityAnnotation] = now.Format(time.RFC3339)
	if err := c.Create(ctx, lease); err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	now = now.Add(61 * time.Second)
	run()
	state = getAgentInstance(t, c, w)
	if state.Status.Activity == nil || !state.Status.Activity.Suspended || state.Spec.OperatingMode == sandbox.Suspended {
		t.Fatal("idle policy did not suspend without rewriting intent")
	}
	current, err := sandbox.Decode(getSandbox(t, c, w))
	if err != nil || current.Spec.OperatingMode != sandbox.Suspended {
		t.Fatal("idle suspension did not reach Core", err)
	}
	source.Spec.Containers[0].Image = "example/agent:v2"
	updateTemplate(t, c, w, source)
	testutil.PublishDefinitions(t, c, scheme)
	lease.Spec.RenewTime.Time = now
	lease.Annotations[activityStateAnnotation], lease.Annotations[lastActivityAnnotation] = "Busy", now.Format(time.RFC3339)
	if err := c.Update(ctx, lease); err != nil {
		t.Fatal(err)
	}
	r = &Reconciler{Client: c, Reader: c, Scheme: scheme, Now: func() time.Time { return now }}
	run()
	state = getAgentInstance(t, c, w)
	current, err = sandbox.Decode(getSandbox(t, c, w))
	if err != nil || current.Spec.OperatingMode != sandbox.Running || state.Status.Definition.Revision != revision || current.Spec.PodTemplate.Spec.Containers[0].Image != "example/agent:v1" {
		t.Fatal("wake-up changed the accepted execution configuration", err)
	}
	state.Spec.OperatingMode = sandbox.Suspended
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	run()
	current, _ = sandbox.Decode(getSandbox(t, c, w))
	if current.Spec.OperatingMode != sandbox.Suspended {
		t.Fatal("busy activity overrode manual suspension")
	}
	state = getAgentInstance(t, c, w)
	state.Spec.Lifecycle.MaxLifetimeSeconds = ptr.To(int32(30))
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	run()
	state = getAgentInstance(t, c, w)
	if state.Status.Lifecycle == nil || state.Status.Lifecycle.ExpiredAt == nil || state.Status.Lifecycle.Reason != "MaxLifetimeReached" {
		t.Fatal("activity or suspension reset maximum execution lifetime")
	}
}
