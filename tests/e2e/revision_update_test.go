//go:build e2e

package e2e_test

import (
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestStableRevisionUpdates(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	group := readyGroup(t, ctx, kube, "revision-updates")
	definition, a := testAgent(group, "versions", agentCommand("v1"))
	definition.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}}}}
	definition.Spec.Container.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}
	definition.Spec.Container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}, PeriodSeconds: 1}
	a.Name = "a"
	b := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: a.Namespace}, Spec: *a.Spec.DeepCopy()}
	create(t, ctx, kube, definition)
	create(t, ctx, kube, a)
	create(t, ctx, kube, b)
	readyAgent(t, ctx, kube, a)
	readyAgent(t, ctx, kube, b)
	instanceUID, claimUID, sandboxUID, pvcUID := a.UID, a.Status.Provisioning.ClaimUID, a.Status.SandboxUID, allocatedWorkspace(t, ctx, kube, a).UID
	r1, bPod := a.Status.Definition.Revision, b.Status.PodUID
	assertRetained := func() {
		t.Helper()
		if a.UID != instanceUID || a.Status.Provisioning.ClaimUID != claimUID || a.Status.SandboxUID != sandboxUID || allocatedWorkspace(t, ctx, kube, a).UID != pvcUID {
			t.Fatal("an update replaced stable Instance/Claim/Sandbox/PVC identity")
		}
		if got := execPod(t, kubeconfig, a.Namespace, a.Status.PodName, "cat", "/workspace/session"); got != "retained-a" {
			t.Fatal("update lost workspace contents", got)
		}
	}
	execPod(t, kubeconfig, a.Namespace, a.Status.PodName, "sh", "-ec", "printf retained-a > /workspace/session; sync")
	oldPod := a.Status.PodUID
	a.Spec.RestartToken = "restart-r1"
	update(t, ctx, kube, a)
	readyAgent(t, ctx, kube, a)
	if a.Status.PodUID == oldPod || a.Status.Definition.Revision != r1 {
		t.Fatal("same-version restart did not replace only the process")
	}
	assertRetained()

	// Publication affects C's provisioning defaults, while A/B keep r1 and their volumes.
	definition.Spec.Container.Args = []string{agentCommand("v2")}
	definition.Spec.Workspace.VolumeClaimTemplate.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("64Mi")
	update(t, ctx, kube, definition)
	await(t, "r2 publication", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return definition.Status.CurrentRevision != r1 && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	r2 := definition.Status.CurrentRevision
	c := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: a.Namespace}, Spec: *b.Spec.DeepCopy()}
	create(t, ctx, kube, c)
	readyAgent(t, ctx, kube, c)
	cPod := c.Status.PodUID
	if c.Status.Definition.Revision != r2 || allocatedWorkspace(t, ctx, kube, c).Spec.Resources.Requests.Storage().Cmp(resource.MustParse("64Mi")) != 0 {
		t.Fatal("C did not use current revision and storage defaults")
	}

	// Hold old-Pod termination across an Operator restart, then request suspension.
	held := &corev1.Pod{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Status.PodName}, held); err != nil {
		t.Fatal(err)
	}
	changePodFinalizer(t, ctx, kube, held, "test.agentrally.io/hold-termination", true)
	a.Spec.Revision = r2
	update(t, ctx, kube, a)
	await(t, "update waiting for old Pod", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(a), a); err != nil {
			return false, err
		}
		err := kube.Get(ctx, client.ObjectKeyFromObject(held), held)
		return a.Status.Update != nil && a.Status.Update.Phase == "Stopping" && held.DeletionTimestamp != nil, err
	})
	kubectl(t, kubeconfig, "-n", "agentrally-system", "rollout", "restart", "deployment/agentrally-operator")
	kubectl(t, kubeconfig, "-n", "agentrally-system", "rollout", "status", "deployment/agentrally-operator", "--timeout=120s")
	a.Spec.OperatingMode = sandbox.Suspended
	update(t, ctx, kube, a)
	changePodFinalizer(t, ctx, kube, held, "test.agentrally.io/hold-termination", false)
	await(t, "r2 prepared while suspended", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(a), a)
		return a.Status.Definition.Revision == r2 && a.Status.Update == nil && conditionTrue(a.Status.Conditions, "Suspended", a.Generation), err
	})
	if a.Status.AppliedRevision != "" || conditionTrue(a.Status.Conditions, "Ready", a.Generation) {
		t.Fatal("suspended preparation was reported as a running upgrade")
	}
	a.Spec.OperatingMode = sandbox.Running
	update(t, ctx, kube, a)
	readyAgent(t, ctx, kube, a)
	assertRetained()
	if got := execPod(t, kubeconfig, a.Namespace, a.Status.PodName, "cat", "/tmp/www/index.html"); got != "v2" {
		t.Fatal("A did not run r2", got)
	}
	if allocatedWorkspace(t, ctx, kube, a).Spec.Resources.Requests.Storage().Cmp(resource.MustParse("32Mi")) != 0 {
		t.Fatal("new defaults resized A's original volume")
	}
	for _, other := range []*v1.AgentInstance{b, c} {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(other), other); err != nil {
			t.Fatal(err)
		}
	}
	if b.Status.Definition.Revision != r1 || b.Status.PodUID != bPod || c.Status.PodUID != cPod {
		t.Fatal("upgrading A changed B or C")
	}

	// A failed new process keeps the allocation and can be repaired by another revision.
	definition.Spec.Container.Image = "docker.io/library/agentrally-e2e-missing:never"
	definition.Spec.Container.ImagePullPolicy = corev1.PullNever
	update(t, ctx, kube, definition)
	await(t, "failed-start revision published", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return definition.Status.CurrentRevision != r2 && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	a.Spec.Revision = definition.Status.CurrentRevision
	update(t, ctx, kube, a)
	await(t, "failed target observed without deleting workspace", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(a), a); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(a.Status.Conditions, "Ready")
		return a.Status.Update != nil && a.Status.Update.Phase == "Starting" && condition != nil && condition.ObservedGeneration == a.Generation && condition.Reason == "ErrImageNeverPull", nil
	})
	a.Spec.Revision = r2
	update(t, ctx, kube, a)
	readyAgent(t, ctx, kube, a)
	assertRetained()
	stablePod := a.Status.PodUID
	kubectl(t, kubeconfig, "-n", "agent-sandbox-system", "rollout", "restart", "deployment/agent-sandbox-controller")
	kubectl(t, kubeconfig, "-n", "agent-sandbox-system", "rollout", "status", "deployment/agent-sandbox-controller", "--timeout=120s")
	update(t, ctx, kube, a) // Repeat the same request after upstream reconciliation restarts.
	readyAgent(t, ctx, kube, a)
	if a.Status.PodUID != stablePod {
		t.Fatal("repeated target restarted the already updated process")
	}
	assertRetained()
	for _, instance := range []*v1.AgentInstance{a, b, c} {
		if err := kube.Delete(ctx, instance); err != nil {
			t.Fatal(err)
		}
	}
	for _, instance := range []*v1.AgentInstance{a, b, c} {
		awaitDeleted(t, ctx, kube, instance)
	}
	if err := kube.Delete(ctx, definition); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, definition)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}
