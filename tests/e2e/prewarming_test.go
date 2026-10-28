//go:build e2e

package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDefinitionPrewarming(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	group := readyGroup(t, ctx, kube, "definition-prewarming")
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 1, MaxTotalReplicas: ptr.To(int32(1))}
	update(t, ctx, kube, group)
	settings := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: group.Status.Namespace}, Data: map[string]string{"VALUE": "initial"}}
	create(t, ctx, kube, settings)
	serve := func(version string) string {
		return "mkdir -p /tmp/www; printf " + version + " > /tmp/www/index.html; exec httpd -f -p 8080 -h /tmp/www"
	}
	definition, cold := testAgent(group, "prepared", serve("v1"))
	definition.Spec.Prewarming = &v1.DefinitionPrewarmingPolicy{Replicas: 0}
	definition.Spec.Container.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}
	definition.Spec.Container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: settings.Name}}}}
	definition.Spec.Container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}, PeriodSeconds: 1}
	create(t, ctx, kube, definition)
	started := time.Now()
	create(t, ctx, kube, cold)
	readyAgent(t, ctx, kube, cold)
	coldElapsed := time.Since(started)
	if cold.Status.Provisioning.LaunchType != "cold" {
		t.Fatal("disabled proactive policy did not allocate cold", cold.Status.Provisioning)
	}
	logStartupEvidence(t, ctx, kube, cold)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition); err != nil {
		t.Fatal(err)
	}
	published := definition.Status.CurrentRevision
	if err := kube.Delete(ctx, cold); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, cold)

	definition.Spec.Prewarming.Replicas = 1
	update(t, ctx, kube, definition)
	preparedDefinition(t, ctx, kube, definition)
	if definition.Status.CurrentRevision != published {
		t.Fatal("capacity scaling changed the application revision")
	}
	instances := &v1.AgentInstanceList{}
	if err := kube.List(ctx, instances, client.InNamespace(definition.Namespace)); err != nil || len(instances.Items) != 0 {
		t.Fatal("capacity preparation required a placeholder Instance", err)
	}
	first := *definition.Status.Prewarming
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := kube.Get(ctx, client.ObjectKey{Namespace: definition.Namespace, Name: first.PoolName}, pool); err != nil {
		t.Fatal(err)
	}
	spares := &unstructured.UnstructuredList{}
	spares.SetGroupVersionKind(sandbox.GroupVersion.WithKind("SandboxList"))
	if err := kube.List(ctx, spares, client.InNamespace(definition.Namespace)); err != nil {
		t.Fatal(err)
	}
	var preparedUID string
	for _, spare := range spares.Items {
		if owner := metav1.GetControllerOf(&spare); owner != nil && owner.UID == pool.GetUID() {
			preparedUID = string(spare.GetUID())
		}
	}
	if preparedUID == "" {
		t.Fatal("Prepared had no real unassigned Sandbox")
	}
	_, warm := testAgent(group, "warm", "")
	warm.Spec.DefinitionRef.Name = definition.Name
	started = time.Now()
	create(t, ctx, kube, warm)
	readyAgent(t, ctx, kube, warm)
	warmElapsed := time.Since(started)
	if warm.Status.Provisioning.LaunchType != "warm" || warm.Status.Provisioning.PoolName != first.PoolName || string(warm.Status.SandboxUID) != preparedUID {
		t.Fatal("matching Instance did not adopt the prepared Sandbox", warm.Status)
	}
	logStartupEvidence(t, ctx, kube, warm)
	logLatencySamples(t, "cold-cached-image", []time.Duration{coldElapsed})
	logLatencySamples(t, "prepared-sandbox", []time.Duration{warmElapsed})
	oldPod, oldRevision := warm.Status.PodUID, warm.Status.Definition.Revision

	settings.Data["VALUE"] = "rotated"
	if err := kube.Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
	await(t, "dependency rotation prepares a new fingerprint", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		state := definition.Status.Prewarming
		return state != nil && state.ConfigurationRevision != first.ConfigurationRevision && conditionTrue(definition.Status.Conditions, "Prepared", definition.Generation), err
	})
	await(t, "old dependency pool stops refilling", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
			return false, err
		}
		replicas, _, err := unstructured.NestedInt64(pool.Object, "spec", "replicas")
		return replicas == 0, err
	})
	if definition.Status.CurrentRevision != published || execPod(t, kubeconfig, warm.Namespace, warm.Status.PodName, "printenv", "VALUE") != "initial" {
		t.Fatal("dependency rotation changed the accepted application or its environment")
	}
	definition.Spec.Container.Args = []string{serve("v2")}
	update(t, ctx, kube, definition)
	preparedDefinition(t, ctx, kube, definition)
	if definition.Status.CurrentRevision == oldRevision {
		t.Fatal("application update did not publish a new revision")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(warm), warm); err != nil || warm.Status.PodUID != oldPod || warm.Status.Definition.Revision != oldRevision {
		t.Fatal("pool retirement replaced an allocated execution", err)
	}

	_, workspace := testAgent(group, "workspace", "")
	workspace.Spec.DefinitionRef.Name = definition.Name
	workspace.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}},
	}}
	create(t, ctx, kube, workspace)
	readyAgent(t, ctx, kube, workspace)
	if workspace.Status.Provisioning.LaunchType != "cold" || workspace.Status.Provisioning.PoolName == definition.Status.Prewarming.PoolName {
		t.Fatal("workspace override consumed stateless prepared capacity")
	}
	workspacePool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := kube.Get(ctx, client.ObjectKey{Namespace: workspace.Namespace, Name: workspace.Status.Provisioning.PoolName}, workspacePool); err != nil {
		t.Fatal(err)
	}
	if replicas, _, _ := unstructured.NestedInt64(workspacePool.Object, "spec", "replicas"); replicas != 0 {
		t.Fatal("instance-specific storage produced spares")
	}
	pvc := allocatedWorkspace(t, ctx, kube, workspace)
	execPod(t, kubeconfig, workspace.Namespace, workspace.Status.PodName, "sh", "-ec", "printf retained > /workspace/canary; sync")

	group.Spec.AllocationPaused = true
	update(t, ctx, kube, group)
	await(t, "pause drains proactive targets", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(definition.Status.Conditions, "Prepared")
		pools := sandbox.ExtensionList("SandboxWarmPool")
		if err := kube.List(ctx, pools, client.InNamespace(definition.Namespace)); err != nil {
			return false, err
		}
		for _, item := range pools.Items {
			replicas, _, _ := unstructured.NestedInt64(item.Object, "spec", "replicas")
			if replicas != 0 {
				return false, nil
			}
		}
		return condition != nil && condition.Reason == "AllocationPaused", nil
	})
	if err := kube.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil || execPod(t, kubeconfig, workspace.Namespace, workspace.Status.PodName, "cat", "/workspace/canary") != "retained" {
		t.Fatal("capacity pause removed allocated data", err)
	}
	for _, instance := range []*v1.AgentInstance{warm, workspace} {
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

func TestPreparedRequiresApplicationProbe(t *testing.T) {
	kube, _ := e2eClient(t)
	ctx := t.Context()
	group := readyGroup(t, ctx, kube, "prepared-probe")
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 1}
	update(t, ctx, kube, group)
	definition, _ := testAgent(group, "not-ready", "sleep 3600")
	definition.Spec.Prewarming = &v1.DefinitionPrewarmingPolicy{Replicas: 1}
	definition.Spec.Container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}}, PeriodSeconds: 1}
	create(t, ctx, kube, definition)
	await(t, "Running spare with failed application readiness", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition); err != nil {
			return false, err
		}
		pods := &corev1.PodList{}
		if err := kube.List(ctx, pods, client.InNamespace(definition.Namespace)); err != nil {
			return false, err
		}
		if len(pods.Items) != 1 || pods.Items[0].Status.Phase != corev1.PodRunning {
			return false, nil
		}
		agentObserved := false
		for _, status := range pods.Items[0].Status.ContainerStatuses {
			if status.Name == "agent" && status.State.Running != nil && !status.Ready {
				agentObserved = true
			}
		}
		if !agentObserved {
			return false, nil
		}
		events := &corev1.EventList{}
		if err := kube.List(ctx, events, client.InNamespace(definition.Namespace)); err != nil {
			return false, err
		}
		probeFailed := false
		for _, event := range events.Items {
			if event.InvolvedObject.UID == pods.Items[0].UID && event.Reason == "Unhealthy" && strings.Contains(event.Message, "Readiness probe failed") {
				probeFailed = true
			}
		}
		state := definition.Status.Prewarming
		return probeFailed && state != nil && state.DesiredReplicas == 1 && state.ReadyReplicas == 0 && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation) && !meta.IsStatusConditionTrue(definition.Status.Conditions, "Prepared"), nil
	})
	definition.Spec.Prewarming.Replicas = 0
	update(t, ctx, kube, definition)
	await(t, "zero request cleans unassigned capacity", func() (bool, error) {
		pools := sandbox.ExtensionList("SandboxWarmPool")
		pods := &corev1.PodList{}
		if err := kube.List(ctx, pools, client.InNamespace(definition.Namespace)); err != nil {
			return false, err
		}
		if err := kube.List(ctx, pods, client.InNamespace(definition.Namespace)); err != nil {
			return false, err
		}
		return len(pools.Items) == 0 && len(pods.Items) == 0, nil
	})
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func preparedDefinition(t *testing.T, ctx context.Context, kube client.Client, definition *v1.AgentDefinition) {
	t.Helper()
	await(t, definition.Name+" proactive capacity", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return conditionTrue(definition.Status.Conditions, "Prepared", definition.Generation), err
	})
}

// Record evidence without logging event messages or assuming missing pull events
// mean zero latency. The driver preloads the fixture image into disposable nodes.
func logStartupEvidence(t *testing.T, ctx context.Context, kube client.Client, instance *v1.AgentInstance) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.PodName}, pod); err != nil {
		t.Fatal(err)
	}
	events := &corev1.EventList{}
	if err := kube.List(ctx, events, client.InNamespace(pod.Namespace)); err != nil {
		t.Fatal(err)
	}
	var pulling, pulled []time.Time
	cached := false
	for _, event := range events.Items {
		if event.InvolvedObject.UID != pod.UID || event.InvolvedObject.FieldPath != "spec.containers{agent}" || event.Count > 1 {
			continue
		}
		stamp := event.EventTime.Time
		if stamp.IsZero() {
			stamp = event.FirstTimestamp.Time
		}
		if stamp.IsZero() {
			continue
		}
		switch event.Reason {
		case "Pulling":
			pulling = append(pulling, stamp)
		case "Pulled":
			pulled = append(pulled, stamp)
			cached = strings.Contains(event.Message, "already present on machine")
		}
	}
	pull := "unavailable"
	if !cached && len(pulling) == 1 && len(pulled) == 1 && pulled[0].After(pulling[0]) {
		pull = pulled[0].Sub(pulling[0]).String()
	}
	t.Logf("startup launch=%s image=%s node=%s podUID=%s created=%s driverPreloaded=true cacheEvent=%t pullInterval=%s", instance.Status.Provisioning.LaunchType,
		pod.Spec.Containers[0].Image, pod.Spec.NodeName, pod.UID, pod.CreationTimestamp.Format(time.RFC3339Nano), cached, pull)
}
