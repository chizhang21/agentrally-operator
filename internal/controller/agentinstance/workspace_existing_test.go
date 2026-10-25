package agentinstance

import (
	"reflect"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExistingWorkspaceSurvivesExecutionReplacement(t *testing.T) {
	group, ns, definition, instance, _ := testutil.Fixture()
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 2}
	definition.Spec.TemplateRef = nil
	definition.Spec.Template = "assistant"
	definition.Spec.Container = &v1.AgentContainerSpec{Image: "example/assistant:v1"}
	instance.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "state"}, Exclusive: true, MountPath: "/workspace"}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: ns.Name, UID: "state-uid"}, Spec: workspaceVolumeSpec(), Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	c, scheme := clientWithPublishedDefinitions(t, definition, group, ns, instance, pvc)
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, instance)
	state := getAgentInstance(t, c, instance)
	if state.Status.Workspace == nil || state.Status.Workspace.UID != pvc.UID || state.Status.VolumeClaims["agentrally-workspace"] != pvc.Name {
		t.Fatal("external workspace was not pinned and reported", state.Status)
	}
	pool := allocationObject(t, c, instance, "SandboxWarmPool", allocation.PoolName(state))
	if replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas"); replicas != 0 {
		t.Fatal("external workspace started unassigned applications")
	}
	if _, err := (&allocation.Manager{Client: c, Reader: c, Scheme: scheme}).Prune(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
		t.Fatal(err)
	}
	if replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas"); replicas != 0 {
		t.Fatal("Group policy re-enabled unsafe workspace spares")
	}
	definition.Spec.Container.Image = "example/assistant:v2"
	saveDefinition(t, c, definition)
	state.Spec.RestartToken = "upgrade"
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		reconcileWithUpstreamSimulation(t, r, state)
	}
	current, err := sandbox.Decode(getSandbox(t, c, instance))
	if err != nil || current.Spec.PodTemplate.Spec.Containers[0].Image != "example/assistant:v2" || len(current.Spec.VolumeClaimTemplates) != 0 {
		t.Fatal("replacement did not retain the external workspace blueprint", err)
	}
	after := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), after); err != nil || after.UID != pvc.UID || !reflect.DeepEqual(after.OwnerReferences, pvc.OwnerReferences) || !reflect.DeepEqual(after.Spec, pvc.Spec) {
		t.Fatal("replacement changed external storage", err)
	}
	// Recreating a PVC under the same name must not silently change user data.
	if err := c.Delete(ctx, after); err != nil {
		t.Fatal(err)
	}
	after.ResourceVersion, after.UID = "", "replacement-pvc"
	if err := c.Create(ctx, after); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, state)
	assertReason(t, getAgentInstance(t, c, instance), "Ready", "WorkspaceBindingChanged")
}

func TestExistingWorkspaceRejectsExecutionOwnedPVC(t *testing.T) {
	group, ns, definition, instance, pod := testutil.Fixture()
	instance.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "private"}, MountPath: "/workspace"}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: ns.Name, UID: "private-pvc", OwnerReferences: []metav1.OwnerReference{{APIVersion: sandbox.GroupVersion.String(), Kind: "Sandbox", Name: "other", UID: "other-uid", Controller: ptr.To(true)}}}, Spec: workspaceVolumeSpec()}
	c, scheme := clientWithPublishedDefinitions(t, definition, group, ns, instance, nativeTemplate(definition, pod), pvc)
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme}
	reconcileAllocation(t, r, instance)
	state := getAgentInstance(t, c, instance)
	assertReason(t, state, "Ready", "WorkspaceOwnershipConflict")
	if state.Status.Provisioning != nil {
		t.Fatal("execution-owned workspace started allocation")
	}
}

func TestExistingWorkspaceBlocksUnavailableVolumes(t *testing.T) {
	for _, scenario := range []string{"missing", "cross-namespace", "terminating", "lost", "block", "volume-name-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			group, ns, definition, instance, source := testutil.Fixture()
			instance.Spec.Workspace = &v1.WorkspaceMount{ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: "state"}, MountPath: "/workspace"}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: ns.Name, UID: "state-uid"}, Spec: workspaceVolumeSpec()}
			objects := []client.Object{group, ns, definition, instance}
			switch scenario {
			case "cross-namespace":
				pvc.Namespace = "other"
			case "terminating":
				now := metav1.Now()
				pvc.DeletionTimestamp = &now
				pvc.Finalizers = []string{"test/retain"}
			case "lost":
				pvc.Status.Phase = corev1.ClaimLost
			case "block":
				pvc.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
			case "volume-name-conflict":
				source.Spec.Volumes = []corev1.Volume{{Name: "agentrally-workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			}
			objects = append(objects, nativeTemplate(definition, source))
			if scenario != "missing" {
				objects = append(objects, pvc)
			}
			c, scheme := clientWithPublishedDefinitions(t, objects...)
			reconcileAllocation(t, &Reconciler{Client: c, Scheme: scheme}, instance)
			state := getAgentInstance(t, c, instance)
			testutil.Condition(t, state.Status.Conditions, "Ready", metav1.ConditionFalse)
			if state.Status.Provisioning != nil || state.Status.Workspace != nil {
				t.Fatal("unavailable workspace started allocation", state.Status)
			}
		})
	}
}
