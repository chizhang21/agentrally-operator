package agentworkspace

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestStableBindingDoesNotRewriteStatus(t *testing.T) {
	ctx := t.Context()
	record := &v1.AgentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace", UID: "record", Generation: 1},
		Spec:       v1.AgentWorkspaceSpec{Namespace: "team", InstanceName: "agent", InstanceUID: "instance"},
		Status:     v1.AgentWorkspaceStatus{Phase: "Bound", PVCUID: "pvc"},
	}
	instance := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "team", UID: "instance"},
		Status: v1.AgentInstanceStatus{Workspace: &v1.WorkspaceBinding{WorkspaceUID: record.UID, UID: record.Status.PVCUID}}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: record.Name, Namespace: "team", UID: "pvc"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	builder, _ := testutil.ClientBuilder(t)
	writes := 0
	kube := builder.WithObjects(record, instance, pvc).WithIndex(&v1.AgentWorkspace{}, instanceUIDIndex, workspaceInstanceUID).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, name string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			writes++
			return c.SubResource(name).Patch(ctx, object, patch, opts...)
		},
	}).Build()
	r := &Reconciler{Client: kube}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(record)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, request.NamespacedName, record); err != nil {
		t.Fatal(err)
	}
	// Make transition timestamps observably old without sleeping or testing the clock.
	for i := range record.Status.Conditions {
		record.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Unix(100, 0))
	}
	if err := kube.Status().Update(ctx, record); err != nil {
		t.Fatal(err)
	}
	stable := record.Status.DeepCopy()
	writes = 0
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, request.NamespacedName, record); err != nil {
		t.Fatal(err)
	}
	if writes != 0 || !reflect.DeepEqual(stable, &record.Status) {
		t.Fatal("stable binding rewrote status or transition timestamps")
	}
	instance.Status.Workspace = nil
	if err := kube.Status().Update(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if err := kube.Delete(ctx, instance); err != nil {
		t.Fatal(err)
	}
	requests := r.mapInstance(ctx, instance)
	if len(requests) != 1 {
		t.Fatal("Instance deletion before workspace pin was not mapped")
	}
	if _, err := r.Reconcile(ctx, requests[0]); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, request.NamespacedName, record); err != nil {
		t.Fatal(err)
	}
	binding := meta.FindStatusCondition(record.Status.Conditions, "BindingReady")
	if writes != 1 || binding == nil || binding.Status != metav1.ConditionFalse || binding.Reason != "ReleasePending" {
		t.Fatal("actual release transition was lost")
	}
}
