package agentinstance

import (
	"context"
	"errors"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestUpdateSupersedesAnIncompleteMetadataHandoff(t *testing.T) {
	group, ns, definition, w, _ := testutil.Fixture()
	definition.Spec.TemplateRef = nil
	definition.Spec.Template = "generic"
	definition.Spec.Container = &v1.AgentContainerSpec{Image: "example/agent:v1"}
	c, scheme := clientWithPublishedDefinitions(t, definition, group, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	w = getAgentInstance(t, c, w)
	originalPool := w.Status.Provisioning.PoolName
	// Reproduce interruption after switching the Claim, before applying PodSpec.
	w.Status.Update = &v1.AgentInstanceUpdate{Configuration: v1.AgentInstanceConfiguration{PoolName: "intermediate", TemplateName: "intermediate", Revision: "intermediate"}, Phase: "Stopping", StartedAt: metav1.Now()}
	if err := c.Status().Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.setClaimPool(ctx, getClaim(t, c, w), "intermediate"); err != nil {
		t.Fatal(err)
	}
	current := getSandbox(t, c, w)
	if err := r.updateClaimedMode(ctx, current, sandbox.Suspended); err != nil {
		t.Fatal(err)
	}
	current.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Suspended", "status": "True", "reason": "PodTerminated", "observedGeneration": current.GetGeneration(), "lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z07:00")}}}
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	// Invalid desired configuration must not be reported as accepted during suspension.
	w.Spec.OperatingMode, w.Spec.Revision = sandbox.Suspended, "missing-revision"
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	w = getAgentInstance(t, c, w)
	testutil.Condition(t, w.Status.Conditions, controller.ConditionConfigurationAccepted, metav1.ConditionFalse)
	if w.Status.Update == nil || w.Status.Update.Configuration.PoolName != "intermediate" {
		t.Fatal("invalid target discarded the accepted intent")
	}
	definition.Spec.Container.Image = "example/agent:v3"
	saveDefinition(t, c, definition)
	testutil.PublishDefinitions(t, c, scheme)
	if err := c.Get(ctx, client.ObjectKeyFromObject(definition), definition); err != nil {
		t.Fatal(err)
	}
	w.Spec.Revision = definition.Status.CurrentRevision
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	pool, _ := sandbox.ClaimPoolName(getClaim(t, c, w))
	if pool != originalPool || getAgentInstance(t, c, w).Status.Update.Configuration.PoolName != "intermediate" {
		t.Fatal("superseding intent lost the intermediate Claim reference before resetting it")
	}
	for range 5 {
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	w = getAgentInstance(t, c, w)
	decoded, err := sandbox.Decode(getSandbox(t, c, w))
	if err != nil || decoded.Spec.PodTemplate.Spec.Containers[0].Image != "example/agent:v3" || decoded.Spec.OperatingMode != sandbox.Suspended || w.Status.Definition.Revision != definition.Status.CurrentRevision {
		t.Fatal("latest valid revision did not converge while staying suspended", err)
	}
}

func TestAllocatedUpdateRetriesWithoutDeletingAllocation(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	oldPod := ownedExecutionPod(t, c, w)
	originalSandbox, originalClaim := getSandbox(t, c, w), getClaim(t, c, w)
	w = getAgentInstance(t, c, w)
	w.Spec.RestartToken = "restart-once"
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	failStarting := true
	guarded := interceptor.NewClient(c, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			t.Fatal("update deleted an allocation resource")
			return nil
		},
		SubResourcePatch: func(ctx context.Context, kube client.Client, resource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if state, ok := obj.(*v1.AgentInstance); ok && resource == "status" && state.Status.Update != nil && state.Status.Update.Phase == "Starting" && failStarting {
				failStarting = false
				return errors.New("interrupted after template write")
			}
			return kube.SubResource(resource).Patch(ctx, obj, patch, opts...)
		},
	})
	r = &Reconciler{Client: guarded, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	state := getAgentInstance(t, c, w)
	if state.Status.Update == nil || state.Status.Update.Phase != "Stopping" {
		t.Fatal("operation not recorded before stopping")
	}
	current := getSandbox(t, c, w)
	decoded, err := sandbox.Decode(current)
	if err != nil || decoded.Spec.OperatingMode != sandbox.Suspended {
		t.Fatal("old execution was not asked to stop", err)
	}
	// A present old Pod blocks writes and resumption, including after a manager restart.
	r = &Reconciler{Client: guarded, Scheme: scheme}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if getAgentInstance(t, c, w).Status.Update.Phase != "Stopping" {
		t.Fatal("update bypassed old Pod termination")
	}
	if err := c.Delete(ctx, oldPod); err != nil {
		t.Fatal(err)
	}
	current.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Suspended", "status": "True", "reason": "PodTerminated", "observedGeneration": current.GetGeneration(), "lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z07:00")}}}
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("injected status failure was not surfaced")
	}
	decoded, _ = sandbox.Decode(getSandbox(t, c, w))
	if decoded.Spec.OperatingMode != sandbox.Suspended || getAgentInstance(t, c, w).Status.Update.Phase != "Stopping" {
		t.Fatal("unrecorded template update resumed execution")
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	pod := ownedExecutionPod(t, c, w)
	pod.UID = "new-pod"
	if err := c.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	nativeReady(t, c, getSandbox(t, c, w))
	for range 3 {
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	state = getAgentInstance(t, c, w)
	if state.Status.Update != nil || state.Status.LastRestartToken != "restart-once" || state.Status.PodUID != "new-pod" {
		t.Fatalf("update did not converge once: %+v", state.Status)
	}
	if getSandbox(t, c, w).GetUID() != originalSandbox.GetUID() || getClaim(t, c, w).GetUID() != originalClaim.GetUID() {
		t.Fatal("update replaced stable allocation identities")
	}
}

func TestUpdateRejectsStorageAndAllocationIdentityChangesBeforeStopping(t *testing.T) {
	for _, scenario := range []string{"volume-spec", "pvc-identity", "sandbox-identity", "claim-identity"} {
		t.Run(scenario, func(t *testing.T) {
			group, ns, definition, w, source := testutil.Fixture()
			spec := workspaceVolumeSpec()
			w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: &spec, MountPath: "/workspace"}
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
			r := &Reconciler{Client: c, Scheme: scheme}
			reconcileWithUpstreamSimulation(t, r, w)
			state := getAgentInstance(t, c, w)
			object := getSandbox(t, c, w)
			pod := ownedExecutionPod(t, c, w)
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: state.Status.VolumeClaims["agentrally-workspace"], Namespace: ns.Name, UID: "original-pvc", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(object, sandbox.GroupVersion.WithKind("Sandbox"))}}, Spec: spec}
			if err := c.Create(ctx, pvc); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, w)
			state = getAgentInstance(t, c, w)
			state.Spec.RestartToken = "request"
			want := "StorageChangeRejected"
			switch scenario {
			case "volume-spec":
				state.Spec.Workspace.VolumeClaimTemplate.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
			case "pvc-identity":
				pvc.UID = types.UID("replacement-pvc")
				if err := c.Update(ctx, pvc); err != nil {
					t.Fatal(err)
				}
				want = "WorkspaceIdentityChanged"
			case "sandbox-identity":
				object = getSandbox(t, c, w)
				object.SetUID("replacement-sandbox")
				if err := c.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
				want = "PoolAllocationLost"
			case "claim-identity":
				claim := getClaim(t, c, w)
				claim.SetUID("replacement-claim")
				if err := c.Update(ctx, claim); err != nil {
					t.Fatal(err)
				}
				want = "PoolAllocationLost"
			}
			if err := c.Update(ctx, state); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, w)
			state = getAgentInstance(t, c, w)
			assertReason(t, state, controller.ConditionReady, want)
			current, _ := sandbox.Decode(getSandbox(t, c, w))
			if current.Spec.OperatingMode != sandbox.Running || state.Status.Update != nil {
				t.Fatal("rejected preflight stopped the original execution")
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp != nil {
				t.Fatal("rejected preflight deleted the original Pod", err)
			}
		})
	}
}
