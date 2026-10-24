package agentinstance

import (
	"context"
	"sync"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/testutil"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestExclusiveWorkspaceRetriesWhenReservationDisappears(t *testing.T) {
	group, ns, _, w, _ := testutil.Fixture()
	w.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "state"}, MountPath: "/workspace", Exclusive: true}
	binding := &v1.WorkspaceBinding{ClaimName: "state", UID: "workspace-uid"}
	lease := workspaceReservation(w, binding)
	c, scheme := testutil.Client(t, group, ns, w, lease)
	reader := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
		if _, ok := object.(*coordinationv1.Lease); ok {
			return apierrors.NewNotFound(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, key.Name)
		}
		return c.Get(ctx, key, object, options...)
	}})
	r := &Reconciler{Client: c, Reader: reader, Scheme: scheme}
	if err := r.bindWorkspace(ctx, w, binding); err == nil {
		t.Fatal("exclusive binding succeeded without an observable reservation")
	}
	if getAgentInstance(t, c, w).Status.Workspace != nil {
		t.Fatal("unreserved workspace identity was accepted")
	}
}

func TestExclusiveWorkspaceArbitratesConcurrentInstances(t *testing.T) {
	group, ns, _, a, _ := testutil.Fixture()
	a.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "state"}, MountPath: "/workspace", Exclusive: true}
	b := a.DeepCopy()
	b.Name, b.UID = "other", "other-instance-uid"
	c, scheme := testutil.Client(t, group, ns, a, b)
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme}
	binding := &v1.WorkspaceBinding{ClaimName: "state", UID: "workspace-uid"}
	var wait sync.WaitGroup
	errors := make([]error, 2)
	for i, instance := range []*v1.AgentInstance{a, b} {
		wait.Add(1)
		go func() { defer wait.Done(); errors[i] = r.bindWorkspace(ctx, instance, binding) }()
	}
	wait.Wait()
	if (errors[0] == nil) == (errors[1] == nil) {
		t.Fatalf("expected one reservation owner, got %v", errors)
	}
	winner, loser := a, b
	if errors[1] == nil {
		winner, loser = b, a
	}
	loser = getAgentInstance(t, c, loser)
	loser.Spec.Workspace.Exclusive = false
	if err := c.Update(ctx, loser); err != nil {
		t.Fatal(err)
	}
	if reason, _ := diagnostics.Classify(r.bindWorkspace(ctx, loser, binding)); reason != "WorkspaceInUse" {
		t.Fatal("shared binding bypassed an exclusive reservation", reason)
	}
	if err := r.releaseWorkspace(ctx, winner); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, winner); err != nil {
		t.Fatal(err)
	}
	loser = getAgentInstance(t, c, loser)
	loser.Spec.Workspace.Exclusive = true
	if err := r.bindWorkspace(ctx, loser, binding); err != nil {
		t.Fatal("cleaned-up reservation was not reusable", err)
	}
}

func TestExclusiveWorkspaceRejectsExistingSharedBinding(t *testing.T) {
	group, ns, _, a, _ := testutil.Fixture()
	a.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "state"}, MountPath: "/workspace", Exclusive: true}
	binding := &v1.WorkspaceBinding{ClaimName: "state", UID: "workspace-uid"}
	other := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns.Name, UID: "shared-uid"}, Status: v1.AgentInstanceStatus{Workspace: binding}}
	c, scheme := testutil.Client(t, group, ns, a, other)
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme}
	if reason, _ := diagnostics.Classify(r.bindWorkspace(ctx, a, binding)); reason != "WorkspaceInUse" {
		t.Fatal("exclusive owner displaced shared binding", reason)
	}
}

func TestUnvalidatedReservationCannotPromoteSharedWorkspace(t *testing.T) {
	group, ns, _, w, _ := testutil.Fixture()
	w.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "state"}, MountPath: "/workspace", Exclusive: true}
	binding := &v1.WorkspaceBinding{ClaimName: "state", UID: "workspace-uid"}
	w.Status.Workspace = binding
	other := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns.Name, UID: "shared-uid"}, Status: v1.AgentInstanceStatus{Workspace: binding}}
	c, scheme := testutil.Client(t, group, ns, w, other)
	failing := true
	guarded := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if _, ok := list.(*v1.AgentInstanceList); ok && failing {
				return apierrors.NewServiceUnavailable("inventory unavailable")
			}
			return c.List(ctx, list, options...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			if _, ok := object.(*coordinationv1.Lease); ok && failing {
				return apierrors.NewServiceUnavailable("rollback unavailable")
			}
			return c.Delete(ctx, object, options...)
		},
	})
	r := &Reconciler{Client: guarded, Reader: guarded, Scheme: scheme}
	if err := r.bindWorkspace(ctx, w, binding); err == nil {
		t.Fatal("unavailable inventory accepted exclusive access")
	}
	failing = false
	if reason, _ := diagnostics.Classify(r.bindWorkspace(ctx, w, binding)); reason != "WorkspaceInUse" {
		t.Fatal("a leftover unvalidated reservation bypassed shared-use protection", reason)
	}
}
