//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The driver creates a disposable kind cluster, installs the real upstream
// controllers and Arally chart, and supplies its separate kubeconfig explicitly.
func TestOperatorLifecycle(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "lifecycle"}, Spec: v1.AgentGroupSpec{
		LimitRange: &corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer,
			Default: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		}}},
	}}
	create(t, ctx, kube, group)
	await(t, "Group readiness", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
	})
	ns := group.Status.Namespace
	workspaceSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse("32Mi"),
		}},
	}
	definition := &v1.AgentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns}, Spec: v1.AgentDefinitionSpec{
		AgentGroupRef: v1.AgentGroupReference{Name: group.Name}, Template: "generic", Security: &v1.AgentSecurity{RunAsUser: ptr.To(int64(1000))},
		Container: &v1.AgentContainerSpec{
			Image: "docker.io/library/busybox:1.37.0", Command: []string{"sh", "-c"},
			Args:           []string{agentCommand("v1")},
			Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}, PeriodSeconds: 1},
			Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi"),
			}},
		},
	}}
	create(t, ctx, kube, definition)
	await(t, "configuration published without execution", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	pods := &corev1.PodList{}
	if err := kube.List(ctx, pods, client.InNamespace(ns)); err != nil || len(pods.Items) != 0 {
		t.Fatal("definition started execution", err)
	}
	claim := &v1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns},
		Spec: v1.AgentInstanceSpec{
			AgentGroupRef: v1.AgentGroupReference{Name: group.Name}, DefinitionRef: v1.AgentDefinitionReference{Name: definition.Name},
			Provisioning: &v1.AgentInstanceProvisioning{ClaimTimeoutSeconds: ptr.To(int32(180))},
			Workspace:    &v1.WorkspaceMount{VolumeClaimTemplate: &workspaceSpec, MountPath: "/workspace"},
			Lifecycle:    &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: ptr.To(int32(0))},
		},
	}
	// The local-path provisioner supplies writable directories in this test
	// cluster. This checks per-Sandbox storage, not arbitrary CSI behavior.
	t.Log("An in-flight allocation survives upstream downtime and manager replacement")
	kubectl(t, kubeconfig, "-n", "agent-sandbox-system", "scale", "deployment/agent-sandbox-controller", "--replicas=0")
	kubectl(t, kubeconfig, "-n", "agent-sandbox-system", "rollout", "status", "deployment/agent-sandbox-controller", "--timeout=120s")
	create(t, ctx, kube, claim)
	refresh := func() error { return kube.Get(ctx, client.ObjectKeyFromObject(claim), claim) }
	await(t, "persisted pending allocation", func() (bool, error) {
		err := refresh()
		state := claim.Status.Provisioning
		return state != nil && state.StartedAt != nil && state.ClaimName != "" && state.CompletedAt == nil, err
	})
	started := claim.Status.Provisioning.StartedAt.DeepCopy()
	kubectl(t, kubeconfig, "-n", "arally-system", "rollout", "restart", "deployment/arally-operator")
	kubectl(t, kubeconfig, "-n", "arally-system", "rollout", "status", "deployment/arally-operator", "--timeout=180s")
	kubectl(t, kubeconfig, "-n", "agent-sandbox-system", "scale", "deployment/agent-sandbox-controller", "--replicas=1")
	awaitReady := func() {
		await(t, "current desired configuration ready", func() (bool, error) {
			err := refresh()
			return conditionTrue(claim.Status.Conditions, "Ready", claim.Generation) &&
				conditionTrue(claim.Status.Conditions, "Available", claim.Generation), err
		})
	}
	awaitReady()
	if !claim.Status.Provisioning.StartedAt.Equal(started) || claim.Status.Provisioning.LaunchType != "cold" {
		t.Fatal("allocation clock or launch provenance did not survive restart", claim.Status.Provisioning)
	}
	if claim.Status.Timings == nil || claim.Status.Timings.PodCreatedAt == nil || claim.Status.Timings.PodReadyAt == nil {
		t.Fatal("startup observations were not persisted")
	}
	await(t, "Group summary", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		summary := group.Status.Summary
		return summary != nil && summary.Instances.Ready == 1 && summary.Instances.Available == 1 && summary.Storage.PVCs == 1, err
	})
	metrics := kubectl(t, kubeconfig, "get", "--raw", "/api/v1/namespaces/arally-system/services/arally-operator-metrics:8080/proxy/metrics")
	if !strings.Contains(metrics, "arally_observation_success 1") || !strings.Contains(metrics, "arally_agent_allocation_duration_seconds_count{launch_type=\"cold\"} 1") {
		t.Fatal("business metrics missing from the existing endpoint")
	}
	pvc := allocatedWorkspace(t, ctx, kube, claim)
	pvcUID := pvc.UID
	oldPod := claim.Status.PodUID
	oldSandbox := sandbox.Object()
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: claim.Status.SandboxName}, oldSandbox); err != nil {
		t.Fatal(err)
	}
	oldSandboxUID := oldSandbox.GetUID()
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: claim.Status.Provisioning.PoolName}, pool); err != nil {
		t.Fatal(err)
	}
	replicas, _, err := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	if err != nil || replicas != 0 {
		t.Fatal("default pool unexpectedly prewarms", replicas, err)
	}
	execPod(t, kubeconfig, ns, claim.Status.PodName, "sh", "-c", "printf persisted-before-restart > /workspace/session")

	t.Log("Group pause blocks new allocations while existing execution remains operable")
	group.Spec.AllocationPaused = true
	update(t, ctx, kube, group)
	blocked := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: ns}, Spec: *claim.Spec.DeepCopy()}
	create(t, ctx, kube, blocked)
	await(t, "paused new allocation without a running budget", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(blocked), blocked)
		ready := meta.FindStatusCondition(blocked.Status.Conditions, "Ready")
		return ready != nil && ready.Reason == "AllocationPaused" && blocked.Status.Provisioning == nil, err
	})

	t.Log("Configuration updates preserve the old healthy execution")
	definition.Spec.Container.Args = []string{agentCommand("v2")}
	update(t, ctx, kube, definition)
	await(t, "new definition publication", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	await(t, "pending update with independent availability", func() (bool, error) {
		err := refresh()
		applied := meta.FindStatusCondition(claim.Status.Conditions, "ConfigurationApplied")
		ready := meta.FindStatusCondition(claim.Status.Conditions, "Ready")
		return claim.Status.Definition.Revision != definition.Status.CurrentRevision && applied != nil && applied.ObservedGeneration == claim.Generation && applied.Reason == "ConfigurationCurrent" &&
			ready != nil && ready.ObservedGeneration == claim.Generation && ready.Status == metav1.ConditionTrue &&
			conditionTrue(claim.Status.Conditions, "Available", claim.Generation), err
	})
	if claim.Status.PodUID != oldPod {
		t.Fatal("a configuration edit replaced the current Pod")
	}

	t.Log("Suspension works while the new configuration is pending")
	claim.Spec.OperatingMode = sandbox.Suspended
	update(t, ctx, kube, claim)
	await(t, "Core-confirmed suspension and Pod removal", func() (bool, error) {
		if err := refresh(); err != nil {
			return false, err
		}
		pods := &corev1.PodList{}
		err := kube.List(ctx, pods, client.InNamespace(ns))
		return len(pods.Items) == 0 && conditionTrue(claim.Status.Conditions, "Suspended", claim.Generation), err
	})
	if claim.Status.Lifecycle != nil && claim.Status.Lifecycle.FinishedAt != nil {
		t.Fatal("suspension started completion TTL", claim.Status.Lifecycle)
	}
	if claim.Status.Timings == nil || claim.Status.Timings.SuspensionStartedAt == nil || claim.Status.Timings.SuspendedAt == nil {
		t.Fatal("suspension observations were not persisted")
	}
	claim.Spec.OperatingMode = sandbox.Running
	update(t, ctx, kube, claim)
	await(t, "resumed old execution", func() (bool, error) {
		err := refresh()
		return conditionTrue(claim.Status.Conditions, "Available", claim.Generation), err
	})
	if claim.Status.Lifecycle != nil && claim.Status.Lifecycle.FinishedAt != nil {
		t.Fatal("resumed execution retained a completion timestamp", claim.Status.Lifecycle)
	}
	if out := execPod(t, kubeconfig, ns, claim.Status.PodName, "cat", "/workspace/session"); out != "persisted-before-restart" {
		t.Fatal("workspace data lost during suspension", out)
	}
	if allocatedWorkspace(t, ctx, kube, claim).UID != pvcUID {
		t.Fatal("suspend/resume replaced the Sandbox workspace")
	}
	group.Spec.AllocationPaused = false
	update(t, ctx, kube, group)
	await(t, "allocation after Group unpause", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(blocked), blocked)
		return conditionTrue(blocked.Status.Conditions, "Ready", blocked.Generation), err
	})
	if err := kube.Delete(ctx, blocked); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, blocked)

	t.Log("Explicit restart applies the new configuration after upstream cleanup")
	claim.Spec.RestartToken = "apply-v2"
	update(t, ctx, kube, claim)
	awaitReady()
	current := sandbox.Object()
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: claim.Status.SandboxName}, current); err != nil {
		t.Fatal(err)
	}
	if current.GetUID() == oldSandboxUID || claim.Status.PodUID == oldPod {
		t.Fatal("explicit restart reused the old execution")
	}
	if out := execPod(t, kubeconfig, ns, claim.Status.PodName, "cat", "/tmp/www/index.html"); out != "v2" {
		t.Fatal("replacement did not apply desired configuration", out)
	}
	if allocatedWorkspace(t, ctx, kube, claim).UID == pvcUID {
		t.Fatal("execution replacement reused the old Sandbox's PVC")
	}
	if out := execPod(t, kubeconfig, ns, claim.Status.PodName, "cat", "/workspace/session"); out != "fresh" {
		t.Fatal("execution replacement did not start with a fresh workspace", out)
	}

	t.Log("Expiry removes execution and its PVC while retaining the instance")
	deadline := metav1.NewTime(time.Now().Add(-time.Minute))
	claim.Spec.Lifecycle = &v1.AgentInstanceLifecycle{ShutdownTime: &deadline, ShutdownPolicy: "Retain"}
	update(t, ctx, kube, claim)
	await(t, "latched expiry", func() (bool, error) {
		err := refresh()
		return conditionTrue(claim.Status.Conditions, "Expired", claim.Generation), err
	})
	awaitExecutionRemoved(t, ctx, kube, ns)
	if err := kube.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, claim)

	t.Log("Deletion of an active instance also waits for real upstream GC")
	claim = &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "delete-active", Namespace: ns}, Spec: claim.Spec}
	claim.Spec.Lifecycle = nil
	create(t, ctx, kube, claim)
	awaitReady()
	if err := kube.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, claim)
	awaitExecutionRemoved(t, ctx, kube, ns)
	verifyWarmWorkspaces(t, ctx, kube, kubeconfig, group, claim.Spec)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func agentCommand(version string) string {
	return "test -e /workspace/session || printf fresh > /workspace/session; mkdir -p /tmp/www; printf " + version + " > /tmp/www/index.html; exec httpd -f -p 8080 -h /tmp/www"
}

func allocatedWorkspace(t *testing.T, ctx context.Context, kube client.Client, claim *v1.AgentInstance) *corev1.PersistentVolumeClaim {
	t.Helper()
	name := claim.Status.VolumeClaims["arally-workspace"]
	if name == "" {
		t.Fatal("allocated workspace is absent from status")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: name}, pvc); err != nil {
		t.Fatal(err)
	}
	current := sandbox.Object()
	if err := kube.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Status.SandboxName}, current); err != nil {
		t.Fatal(err)
	}
	owner := metav1.GetControllerOf(pvc)
	if owner == nil || owner.Kind != "Sandbox" || owner.UID != current.GetUID() {
		t.Fatal("workspace PVC is not owned by the allocated Sandbox")
	}
	return pvc
}

func verifyWarmWorkspaces(t *testing.T, ctx context.Context, kube client.Client, kubeconfig string, group *v1.AgentGroup, spec v1.AgentInstanceSpec) {
	t.Helper()
	namespace := group.Status.Namespace
	t.Log("Warm allocation transfers independent pre-provisioned PVCs")
	spec.Lifecycle, spec.Provisioning, spec.RestartToken = nil, nil, ""
	observed := map[string][]time.Duration{}
	ready := func(claim *v1.AgentInstance) {
		await(t, claim.Name+" readiness", func() (bool, error) {
			err := kube.Get(ctx, client.ObjectKeyFromObject(claim), claim)
			return conditionTrue(claim.Status.Conditions, "Ready", claim.Generation), err
		})
		launch := claim.Status.Provisioning.LaunchType
		observed[launch] = append(observed[launch], time.Since(claim.CreationTimestamp.Time))
	}
	seed := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "warm-seed", Namespace: namespace}, Spec: *spec.DeepCopy()}
	create(t, ctx, kube, seed)
	ready(seed)
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: seed.Status.Provisioning.PoolName}, pool); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(group), group); err != nil {
		t.Fatal(err)
	}
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 2, IdleTTLSeconds: 300}
	update(t, ctx, kube, group)
	warmNames := map[string]bool{}
	await(t, "two ready spare Sandboxes with their own PVCs", func() (bool, error) {
		spares := &unstructured.UnstructuredList{}
		spares.SetGroupVersionKind(sandbox.GroupVersion.WithKind("SandboxList"))
		if err := kube.List(ctx, spares, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		warmNames = map[string]bool{}
		for _, object := range spares.Items {
			if !metav1.IsControlledBy(&object, pool) {
				continue
			}
			current, err := sandbox.Decode(&object)
			if err != nil {
				return false, err
			}
			if !conditionTrue(current.Status.Conditions, "Ready", object.GetGeneration()) {
				continue
			}
			pvc := &corev1.PersistentVolumeClaim{}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "arally-workspace-" + object.GetName()}, pvc); err != nil {
				return false, err
			}
			if pvc.Status.Phase == corev1.ClaimBound {
				warmNames[object.GetName()] = true
			}
		}
		return len(warmNames) == 2, nil
	})
	agents := []*v1.AgentInstance{
		{ObjectMeta: metav1.ObjectMeta{Name: "warm-one", Namespace: namespace}, Spec: *spec.DeepCopy()},
		{ObjectMeta: metav1.ObjectMeta{Name: "warm-two", Namespace: namespace}, Spec: *spec.DeepCopy()},
	}
	for _, claim := range agents {
		create(t, ctx, kube, claim)
	}
	seen := map[string]bool{allocatedWorkspace(t, ctx, kube, seed).Name: true}
	preexisting := false
	for _, claim := range agents {
		ready(claim)
		if claim.Status.Provisioning.PoolName != pool.GetName() {
			t.Fatal("matching workspace specs did not share the prewarmed pool")
		}
		object := sandbox.Object()
		if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: claim.Status.SandboxName}, object); err != nil {
			t.Fatal(err)
		}
		if object.GetLabels()["agents.x-k8s.io/launch-type"] != "warm" {
			t.Fatal("PVC configuration forced cold allocation")
		}
		if claim.Status.Provisioning.LaunchType != "warm" {
			t.Fatal("warm provenance missing from instance status")
		}
		preexisting = preexisting || warmNames[object.GetName()]
		pvc := allocatedWorkspace(t, ctx, kube, claim)
		if seen[pvc.Name] {
			t.Fatal("different instances share a workspace PVC")
		}
		seen[pvc.Name] = true
	}
	if !preexisting {
		t.Fatal("allocation did not adopt any of the pre-provisioned instances")
	}
	for _, launch := range []string{"cold", "warm"} {
		logLatencySamples(t, launch+"-create-to-ready-observed", observed[launch])
	}
	execPod(t, kubeconfig, namespace, agents[0].Status.PodName, "sh", "-c", "printf private > /workspace/private")
	if out := execPod(t, kubeconfig, namespace, agents[1].Status.PodName, "sh", "-c", "test ! -e /workspace/private && echo isolated"); out != "isolated" {
		t.Fatal("workspace contents leaked between allocated agents", out)
	}
	t.Log("An external pool can reuse the verified Group snapshot")
	external := sandbox.ExtensionObject("SandboxWarmPool")
	external.SetName("external")
	external.SetNamespace(namespace)
	external.Object["spec"] = map[string]interface{}{"sandboxTemplateRef": map[string]interface{}{"name": seed.Status.Provisioning.TemplateName}, "replicas": int64(0)}
	create(t, ctx, kube, external)
	externalClaim := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: namespace}, Spec: *spec.DeepCopy()}
	externalClaim.Spec.Provisioning = &v1.AgentInstanceProvisioning{WarmPoolRef: &v1.WarmPoolReference{Name: "external"}}
	create(t, ctx, kube, externalClaim)
	ready(externalClaim)
	if externalClaim.Status.Provisioning.TemplateName != seed.Status.Provisioning.TemplateName {
		t.Fatal("external pool did not use the verified snapshot")
	}
	if err := kube.Delete(ctx, externalClaim); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, externalClaim)
	if err := kube.Delete(ctx, external); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, external)
	for _, claim := range append(agents, seed) {
		if err := kube.Delete(ctx, claim); err != nil {
			t.Fatal(err)
		}
		awaitDeleted(t, ctx, kube, claim)
	}
	await(t, "idle retained capacity", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
			return false, err
		}
		return pool.GetAnnotations()["arally.io/unused-since"] != "" && pool.GetDeletionTimestamp() == nil, nil
	})
	// The caller deletes the Group next: its finalizer must bypass the five-minute
	// idle retention and wait for real upstream reclamation before removing the namespace.
}

func conditionTrue(conditions []metav1.Condition, kind string, generation int64) bool {
	c := meta.FindStatusCondition(conditions, kind)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
}

func e2eClient(t *testing.T) (client.Client, string) {
	t.Helper()
	kubeconfig := os.Getenv("ARALLY_E2E_KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("run make e2e to create an isolated cluster")
	}
	raw, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil || !strings.HasPrefix(raw.CurrentContext, "kind-arally-e2e-") {
		t.Fatal("E2E requires the driver's disposable kind context", err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 30 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, networkingv1.AddToScheme, nodev1.AddToScheme, storagev1.AddToScheme, v1.AddToScheme, sandbox.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return kube, kubeconfig
}

func await(t *testing.T, description string, check func() (bool, error)) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(t.Context(), time.Second, 4*time.Minute, true,
		func(context.Context) (bool, error) { return check() }); err != nil {
		t.Fatalf("%s: %v", description, err)
	}
}

func create(t *testing.T, ctx context.Context, kube client.Client, object client.Object) {
	t.Helper()
	if err := kube.Create(ctx, object); err != nil {
		t.Fatal(err)
	}
}

func update(t *testing.T, ctx context.Context, kube client.Client, object client.Object) {
	t.Helper()
	desired := object.DeepCopyObject().(client.Object)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			return err
		}
		switch desired := desired.(type) {
		case *v1.AgentDefinition:
			object.(*v1.AgentDefinition).Spec = *desired.Spec.DeepCopy()
		case *v1.AgentInstance:
			object.(*v1.AgentInstance).Spec = *desired.Spec.DeepCopy()
		case *v1.AgentGroup:
			object.(*v1.AgentGroup).Spec = *desired.Spec.DeepCopy()
		case *v1.ClusterCapabilities:
			object.(*v1.ClusterCapabilities).Spec = *desired.Spec.DeepCopy()
		default:
			t.Fatalf("unsupported E2E intent update: %T", object)
		}
		return kube.Update(ctx, object)
	}); err != nil {
		t.Fatal(err)
	}
}

func awaitDeleted(t *testing.T, ctx context.Context, kube client.Client, object client.Object) {
	t.Helper()
	await(t, fmt.Sprintf("%T %s deletion", object, object.GetName()), func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(object), object)
		return err != nil && client.IgnoreNotFound(err) == nil, client.IgnoreNotFound(err)
	})
}

func awaitExecutionRemoved(t *testing.T, ctx context.Context, kube client.Client, namespace string) {
	t.Helper()
	await(t, "Pod, Sandbox, native Claim and PVC cleanup", func() (bool, error) {
		pods := &corev1.PodList{}
		pvcs := &corev1.PersistentVolumeClaimList{}
		claims := sandbox.ExtensionList("SandboxClaim")
		sandboxes := &unstructured.UnstructuredList{}
		sandboxes.SetGroupVersionKind(sandbox.GroupVersion.WithKind("SandboxList"))
		for _, list := range []client.ObjectList{pods, claims, sandboxes, pvcs} {
			if err := kube.List(ctx, list, client.InNamespace(namespace)); err != nil {
				return false, err
			}
		}
		return len(pods.Items)+len(claims.Items)+len(sandboxes.Items)+len(pvcs.Items) == 0, nil
	})
}

func execPod(t *testing.T, kubeconfig, namespace, pod string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", namespace, "exec", pod, "--"}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("exec test Pod: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func kubectl(t *testing.T, kubeconfig string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func deleteDefinitions(t *testing.T, ctx context.Context, kube client.Client, group *v1.AgentGroup) {
	t.Helper()
	list := &v1.AgentDefinitionList{}
	if err := kube.List(ctx, list, client.InNamespace(group.Status.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		object := &list.Items[i]
		if err := kube.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
		awaitDeleted(t, ctx, kube, object)
	}
}
