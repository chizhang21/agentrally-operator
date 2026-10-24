//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRuntimePolicyAndIdleLifecycle(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	group := readyGroup(t, ctx, kube, "runtime-policy")
	class := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "policy-runc"}, Handler: "runc"}
	create(t, ctx, kube, class)
	t.Cleanup(func() { _ = kube.Delete(ctx, class) })
	group.Spec.ExecutionPolicy = &v1.ExecutionPolicy{AllowedRuntimeClasses: []string{class.Name}, RequireImageDigest: true}
	update(t, ctx, kube, group)
	definition, instance := testAgent(group, "idle-agent", agentCommand("v1"))
	definition.Spec.Template = "assistant"
	definition.Spec.Container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}, PeriodSeconds: 1}
	instance.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}}}}
	instance.Spec.Lifecycle = &v1.AgentInstanceLifecycle{IdleTimeoutSeconds: ptr.To(int32(5)), ActivityLeaseRef: &v1.ActivityLeaseReference{Name: "activity"}}
	create(t, ctx, kube, definition)
	create(t, ctx, kube, instance)
	await(t, "missing RuntimeClass denied", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		condition := meta.FindStatusCondition(instance.Status.Conditions, "CapabilityValidated")
		return condition != nil && condition.Reason == "RuntimeClassDenied" && instance.Status.Provisioning == nil, err
	})
	digest := os.Getenv("E2E_DIGEST_IMAGE")
	if !strings.Contains(digest, "@sha256:") {
		t.Fatal("driver must provide the pulled workload image digest")
	}
	definition.Spec.Container.RuntimeClassName, definition.Spec.Container.Image = &class.Name, digest
	update(t, ctx, kube, definition)
	previous := instance.Status.Definition.Revision
	await(t, "digest-pinned definition published", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return definition.Status.CurrentRevision != previous && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	instance.Spec.RestartToken = "accept-pinned-definition"
	update(t, ctx, kube, instance)
	readyAgent(t, ctx, kube, instance)
	accepted, execution := instance.Status.Definition.Revision, instance.Status.SandboxUID
	execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "sh", "-ec", "printf idle-retained > /workspace/canary; sync")
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "activity", Namespace: instance.Namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "AgentInstance", Name: instance.Name, UID: instance.UID, Controller: ptr.To(true)}}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(string(execution)), LeaseDurationSeconds: ptr.To(int32(120))}}
	signal := func(state string) {
		t.Helper()
		now := time.Now().UTC()
		lease.Spec.RenewTime = &metav1.MicroTime{Time: now}
		lease.Annotations = map[string]string{"arally.io/activity-state": state, "arally.io/last-activity-time": now.Format(time.RFC3339Nano)}
		if lease.ResourceVersion == "" {
			create(t, ctx, kube, lease)
		} else {
			if err := kube.Update(ctx, lease); err != nil {
				t.Fatal(err)
			}
		}
	}
	var resume []time.Duration
	for attempt := range 3 {
		oldPod := instance.Status.PodUID
		signal("Idle")
		await(t, "automatic idle suspension", func() (bool, error) {
			err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
			return instance.Status.Activity != nil && instance.Status.Activity.Suspended && conditionTrue(instance.Status.Conditions, "Suspended", instance.Generation), err
		})
		if attempt == 0 {
			definition.Spec.Container.Args = []string{agentCommand("v2")}
			update(t, ctx, kube, definition)
			await(t, "new version while idle", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
				return definition.Status.CurrentRevision != accepted, err
			})
		}
		start := time.Now()
		signal("Busy")
		await(t, "activity wake-up with a new process", func() (bool, error) {
			err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
			return conditionTrue(instance.Status.Conditions, "Ready", instance.Generation) && instance.Status.PodUID != oldPod, err
		})
		resume = append(resume, time.Since(start))
		if instance.Status.SandboxUID != execution || instance.Status.Definition.Revision != accepted {
			t.Fatal("automatic wake-up replaced execution or upgraded the accepted version")
		}
		if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "cat", "/workspace/canary"); got != "idle-retained" {
			t.Fatal("idle suspension lost workspace data", got)
		}
		if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "cat", "/tmp/www/index.html"); got != "v1" {
			t.Fatal("wake-up ran a new application version", got)
		}
	}
	logLatencySamples(t, "resume-observed", resume)
	instance.Spec.OperatingMode = sandbox.Suspended
	update(t, ctx, kube, instance)
	signal("Busy")
	await(t, "manual suspension wins over busy activity", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		return conditionTrue(instance.Status.Conditions, "Suspended", instance.Generation), err
	})
	instance.Spec.Lifecycle.MaxLifetimeSeconds = ptr.To(int32(1))
	update(t, ctx, kube, instance)
	await(t, "maximum lifetime cleanup while suspended", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		return instance.Status.Lifecycle != nil && instance.Status.Lifecycle.Reason == "MaxLifetimeReached" && conditionTrue(instance.Status.Conditions, "CleanupReady", instance.Generation), err
	})
	if err := kube.Delete(ctx, instance); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, instance)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func TestGroupSpareBudget(t *testing.T) {
	kube, _ := e2eClient(t)
	ctx := context.Background()
	group := readyGroup(t, ctx, kube, "spare-budget")
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 2, MaxTotalReplicas: ptr.To(int32(2)), IdleTTLSeconds: 60}
	update(t, ctx, kube, group)
	var instances []*v1.AgentInstance
	for _, name := range []string{"first", "second"} {
		definition, instance := testAgent(group, name, "sleep 3600")
		create(t, ctx, kube, definition)
		create(t, ctx, kube, instance)
		instances = append(instances, instance)
	}
	for _, instance := range instances {
		readyAgent(t, ctx, kube, instance)
	}
	await(t, "two ready spares across two configurations", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return group.Status.Summary != nil && group.Status.Summary.Pools.DesiredSpareReplicas == 2 && group.Status.Summary.Pools.ReadySpareReplicas == 2 && conditionTrue(group.Status.Conditions, "CapacityReady", group.Generation), err
	})
	for _, instance := range instances {
		if err := kube.Delete(ctx, instance); err != nil {
			t.Fatal(err)
		}
		awaitDeleted(t, ctx, kube, instance)
	}
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func TestKataRuntimeLifecycle(t *testing.T) {
	name := os.Getenv("E2E_RUNTIME_CLASS")
	if name == "" {
		t.Skip("set E2E_RUNTIME_CLASS with an installed Kata runtime in the disposable cluster")
	}
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	class := &nodev1.RuntimeClass{}
	if err := kube.Get(ctx, client.ObjectKey{Name: name}, class); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(class.Handler), "kata") {
		t.Fatal("Kata profile requires an actual Kata runtime handler, not a runc alias")
	}
	group := readyGroup(t, ctx, kube, "kata-runtime")
	group.Spec.ExecutionPolicy = &v1.ExecutionPolicy{AllowedRuntimeClasses: []string{name}}
	update(t, ctx, kube, group)
	definition, instance := testAgent(group, "kata", "sleep 3600")
	definition.Spec.Container.RuntimeClassName = &name
	instance.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}}}}
	create(t, ctx, kube, definition)
	create(t, ctx, kube, instance)
	readyAgent(t, ctx, kube, instance)
	t.Logf("Selected Kata handler %s executed with kernel %s", class.Handler, execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "uname", "-r"))
	execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "sh", "-ec", "printf kata-workspace > /workspace/canary; sync")
	oldPod := instance.Status.PodUID
	instance.Spec.OperatingMode = sandbox.Suspended
	update(t, ctx, kube, instance)
	await(t, "Kata suspension and Pod removal", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		return conditionTrue(instance.Status.Conditions, "Suspended", instance.Generation), err
	})
	instance.Spec.OperatingMode = sandbox.Running
	update(t, ctx, kube, instance)
	readyAgent(t, ctx, kube, instance)
	if instance.Status.PodUID == oldPod {
		t.Fatal("Kata resume did not recreate the process")
	}
	if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "cat", "/workspace/canary"); got != "kata-workspace" {
		t.Fatal("Kata resume lost workspace state", got)
	}
	instance.Spec.RestartToken = "replace-kata-execution"
	update(t, ctx, kube, instance)
	await(t, "Kata replacement", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		return instance.Status.LastRestartToken == instance.Spec.RestartToken && conditionTrue(instance.Status.Conditions, "Ready", instance.Generation), err
	})
	pod := &corev1.Pod{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.PodName}, pod); err != nil {
		t.Fatal(err)
	}
	const hold = "e2e.arally.io/hold-cleanup"
	changePodFinalizer(t, ctx, kube, pod, hold, true)
	if err := kube.Delete(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(t.Context(), time.Second, 7*time.Minute, true, func(context.Context) (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(instance.Status.Conditions, "CleanupReady")
		return condition != nil && strings.HasSuffix(condition.Reason, "Stalled"), nil
	}); err != nil {
		t.Fatal("held Kata cleanup was not diagnosed", err)
	}
	changePodFinalizer(t, ctx, kube, pod, hold, false)
	awaitDeleted(t, ctx, kube, instance)
	awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func changePodFinalizer(t *testing.T, ctx context.Context, kube client.Client, pod *corev1.Pod, name string, present bool) {
	t.Helper()
	uid := pod.UID
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return err
		}
		if pod.UID != uid {
			t.Fatal("execution Pod changed during cleanup hold")
		}
		if present && !slices.Contains(pod.Finalizers, name) {
			pod.Finalizers = append(pod.Finalizers, name)
		}
		if !present {
			pod.Finalizers = slices.DeleteFunc(pod.Finalizers, func(value string) bool { return value == name })
		}
		return kube.Update(ctx, pod)
	}); err != nil {
		t.Fatal(err)
	}
}
