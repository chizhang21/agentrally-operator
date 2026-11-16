//go:build e2e

package e2e_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGroupSecurityBoundaries(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	restricted := readyGroup(t, ctx, kube, "restricted-agents")
	baseline := readyGroup(t, ctx, kube, "root-agents")
	baseline.Spec.PodSecurity = "Baseline"
	update(t, ctx, kube, baseline)
	await(t, "independent Baseline Group", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(baseline), baseline)
		return conditionTrue(baseline.Status.Conditions, "Ready", baseline.Generation), err
	})
	class := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "security-runc"}, Handler: "runc"}
	create(t, ctx, kube, class)
	digest := workloadDigest(t)

	t.Log("Default built-in and native Agents retain non-root process restrictions and no API token")
	definition, ordinary := testAgent(restricted, "ordinary", "sleep 3600")
	create(t, ctx, kube, definition)
	create(t, ctx, kube, ordinary)
	readyAgent(t, ctx, kube, ordinary)
	verifyRestrictedProcess(t, kubeconfig, ordinary)
	pod := restrictedSecurityPod(digest)
	_, definition, native := nativeSecurityAgent(t, kube, restricted, "native", pod)
	create(t, ctx, kube, definition)
	create(t, ctx, kube, native)
	readyAgent(t, ctx, kube, native)
	verifyRestrictedProcess(t, kubeconfig, native)

	t.Log("Built-in root needs an independent Baseline Group and an administrator-created account")
	for _, scenario := range []struct {
		name, profile, account, reason string
		group                          *v1.AgentGroup
		apiDenied                      bool
	}{
		{"restricted-root", "Restricted", "", "Restricted cannot run as UID 0", restricted, true},
		{"restricted-anyuid", "AnyUID", "bootstrap", "SecurityPolicyDenied", restricted, false},
		{"no-account", "AnyUID", "", "AnyUID requires a dedicated serviceAccountName", baseline, true},
	} {
		definition, instance := testAgent(scenario.group, scenario.name, "sleep 3600")
		definition.Spec.Security = &v1.AgentSecurity{Profile: scenario.profile, RunAsUser: ptr.To(int64(0)), ServiceAccountName: scenario.account}
		if scenario.apiDenied {
			if err := kube.Create(ctx, definition); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), scenario.reason) {
				t.Fatalf("%s was not rejected by AgentRally's CRD validation: %v", scenario.name, err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(definition), &v1.AgentDefinition{}); !apierrors.IsNotFound(err) {
				t.Fatal("rejected Definition was persisted", scenario.name, err)
			}
			continue
		}
		create(t, ctx, kube, definition)
		create(t, ctx, kube, instance)
		waitInstanceReason(t, kube, instance, scenario.reason)
		assertNoAllocation(t, kube, instance)
	}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: baseline.Status.Namespace}}
	rootDefinition, root := testAgent(baseline, "bootstrap", `test "$(id -u)" = 0
mkdir -p /workspace/bootstrap
chown 65534:65534 /workspace/bootstrap
exec su -s /bin/sh -c 'id -u > /workspace/bootstrap/uid; exec sleep 3600' nobody`)
	rootDefinition.Spec.Template = "assistant"
	rootDefinition.Spec.Security = &v1.AgentSecurity{Profile: "AnyUID", ServiceAccountName: account.Name, RunAsUser: ptr.To(int64(0))}
	rootDefinition.Spec.Container.ReadinessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-ec", `test "$(cat /workspace/bootstrap/uid)" = 65534`}}},
		PeriodSeconds: 1,
	}
	create(t, ctx, kube, rootDefinition)
	create(t, ctx, kube, root)
	waitInstanceReason(t, kube, root, "DependencyNotFound")
	assertNoAllocation(t, kube, root)
	create(t, ctx, kube, account)
	readyAgent(t, ctx, kube, root)
	if got := execPod(t, kubeconfig, root.Namespace, root.Status.PodName, "cat", "/workspace/bootstrap/uid"); got != "65534" {
		t.Fatal("root bootstrap did not drop to the application user", got)
	}
	pod = restrictedSecurityPod(digest)
	pod.Spec.ServiceAccountName = account.Name
	pod.Spec.SecurityContext.RunAsNonRoot, pod.Spec.SecurityContext.RunAsUser = ptr.To(false), ptr.To(int64(0))
	_, definition, nativeRoot := nativeSecurityAgent(t, kube, baseline, "native-root", pod)
	create(t, ctx, kube, definition)
	create(t, ctx, kube, nativeRoot)
	readyAgent(t, ctx, kube, nativeRoot)
	if got := kubectl(t, kubeconfig, "-n", nativeRoot.Namespace, "exec", nativeRoot.Status.PodName, "-c", "agent", "--", "id", "-u"); strings.TrimSpace(got) != "0" {
		t.Fatal("native root did not execute the declared UID", got)
	}

	t.Log("Real namespace admission rejects unsafe main, sidecar and init containers, including with a RuntimeClass")
	for _, scenario := range []struct {
		name   string
		group  *v1.AgentGroup
		mutate func(*corev1.PodSpec)
	}{
		{"main-root", restricted, func(p *corev1.PodSpec) {
			p.RuntimeClassName = &class.Name
			p.Containers[0].SecurityContext.RunAsUser = ptr.To(int64(0))
		}},
		{"sidecar-escalation", restricted, func(p *corev1.PodSpec) {
			p.Containers[1].SecurityContext.AllowPrivilegeEscalation = ptr.To(true)
		}},
		{"init-capability", restricted, func(p *corev1.PodSpec) {
			p.InitContainers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		}},
		{"baseline-privileged", baseline, func(p *corev1.PodSpec) {
			p.ServiceAccountName = account.Name
			p.Containers[0].SecurityContext.Privileged = ptr.To(true)
			p.Containers[0].SecurityContext.AllowPrivilegeEscalation = ptr.To(true)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			pod := restrictedSecurityPod(digest)
			scenario.mutate(&pod.Spec)
			source, definition, instance := nativeSecurityAgent(t, kube, scenario.group, scenario.name, pod)
			create(t, ctx, kube, definition)
			create(t, ctx, kube, instance)
			waitInstanceReason(t, kube, instance, "PodAdmissionDenied")
			verifyAdmissionDenial(t, kube, instance)
			if scenario.name != "main-root" {
				return
			}
			// Fix the requested Pod; do not relax the namespace to recover.
			originalSandbox, previous := instance.Status.SandboxUID, instance.Status.Definition.Revision
			base := source.DeepCopy()
			pod.Spec.Containers[0].SecurityContext.RunAsUser = nil
			fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
			if err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedMap(source.Object, fields, "spec", "podTemplate"); err != nil {
				t.Fatal(err)
			}
			if err := kube.Patch(ctx, source, client.MergeFrom(base)); err != nil {
				t.Fatal(err)
			}
			await(t, "corrected security revision", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
				return definition.Status.CurrentRevision != previous && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
			})
			instance.Spec.Revision = definition.Status.CurrentRevision
			update(t, ctx, kube, instance)
			readyAgent(t, ctx, kube, instance)
			if instance.Status.SandboxUID != originalSandbox {
				t.Fatal("fixing admission replaced the Sandbox allocation")
			}
			verifyRestrictedProcess(t, kubeconfig, instance)
		})
	}

	t.Log("Image digest policy covers the resolved agent, sidecar and init containers")
	restricted.Spec.ExecutionPolicy = &v1.ExecutionPolicy{RequireImageDigest: true}
	update(t, ctx, kube, restricted)
	for _, name := range []string{"builtin-tag", "sidecar-tag", "init-tag"} {
		definition, instance := testAgent(restricted, name, "sleep 3600")
		if name != "builtin-tag" {
			pod := restrictedSecurityPod(digest)
			if name == "sidecar-tag" {
				pod.Spec.Containers[1].Image = "docker.io/library/busybox:1.37.0"
			} else {
				pod.Spec.InitContainers[0].Image = "docker.io/library/busybox:1.37.0"
			}
			_, definition, instance = nativeSecurityAgent(t, kube, restricted, name, pod)
		}
		create(t, ctx, kube, definition)
		create(t, ctx, kube, instance)
		waitInstanceReason(t, kube, instance, "ImageDigestRequired")
		assertNoAllocation(t, kube, instance)
	}
	_, definition, pinned := nativeSecurityAgent(t, kube, restricted, "all-pinned", restrictedSecurityPod(digest))
	create(t, ctx, kube, definition)
	create(t, ctx, kube, pinned)
	readyAgent(t, ctx, kube, pinned)
	verifyRestrictedProcess(t, kubeconfig, pinned)

	for group, level := range map[*v1.AgentGroup]string{restricted: "restricted", baseline: "baseline"} {
		ns := &corev1.Namespace{}
		if err := kube.Get(ctx, client.ObjectKey{Name: group.Status.Namespace}, ns); err != nil {
			t.Fatal(err)
		}
		if ns.Labels["pod-security.kubernetes.io/enforce"] != level {
			t.Fatal("execution changed namespace admission", group.Name, ns.Labels)
		}
		accounts := &corev1.ServiceAccountList{}
		if err := kube.List(ctx, accounts, client.InNamespace(ns.Name)); err != nil {
			t.Fatal(err)
		}
		for _, current := range accounts.Items {
			if current.Name != "default" && !(group == baseline && current.Name == account.Name && current.UID == account.UID) {
				t.Fatal("execution created an unexpected account", current.Name)
			}
		}
		removeScenarioGroup(t, kube, group)
	}
	if err := kube.Delete(ctx, class); err != nil {
		t.Fatal(err)
	}
}

func workloadDigest(t *testing.T) string {
	t.Helper()
	image := os.Getenv("E2E_DIGEST_IMAGE")
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("driver must provide the pulled workload image digest")
	}
	return image
}

// A native Pod with independent regular/init containers, without using AgentRally's renderer.
func restrictedSecurityPod(image string) *corev1.PodTemplateSpec {
	container := corev1.Container{Name: "agent", Image: image, Command: []string{"sleep", "3600"},
		Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
	}
	sidecar, init := *container.DeepCopy(), *container.DeepCopy()
	sidecar.Name, init.Name, init.Command = "security-detail-canary", "prepare", []string{"true"}
	return &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		NodeSelector: map[string]string{corev1.LabelOSStable: "linux"}, TerminationGracePeriodSeconds: ptr.To(int64(1)),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers:      []corev1.Container{container, sidecar}, InitContainers: []corev1.Container{init},
	}}
}

func nativeSecurityAgent(t *testing.T, kube client.Client, group *v1.AgentGroup, name string, pod *corev1.PodTemplateSpec) (*unstructured.Unstructured, *v1.AgentDefinition, *v1.AgentInstance) {
	t.Helper()
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		t.Fatal(err)
	}
	source := sandbox.ExtensionObject("SandboxTemplate")
	source.SetName(name)
	source.SetNamespace(group.Status.Namespace)
	source.SetLabels(map[string]string{"arally.io/category": "generic"})
	source.Object["spec"] = map[string]interface{}{
		"networkPolicyManagement": "Unmanaged", "envVarsInjectionPolicy": "Disallowed",
		"volumeClaimTemplatesPolicy": "Disallowed", "service": true, "podTemplate": fields,
	}
	create(t, t.Context(), kube, source)
	definition, instance := testAgent(group, name, "")
	definition.Spec.Template, definition.Spec.Container, definition.Spec.Security = "", nil, nil
	definition.Spec.TemplateRef = &v1.SandboxTemplateReference{Name: name}
	return source, definition, instance
}

func waitInstanceReason(t *testing.T, kube client.Client, instance *v1.AgentInstance, reason string) {
	t.Helper()
	await(t, instance.Name+" rejected: "+reason, func() (bool, error) {
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(instance), instance); err != nil {
			return false, err
		}
		if meta.IsStatusConditionTrue(instance.Status.Conditions, "Ready") {
			t.Fatalf("rejected instance %s became Ready", instance.Name)
		}
		for _, condition := range instance.Status.Conditions {
			if condition.ObservedGeneration == instance.Generation && condition.Status == metav1.ConditionFalse && condition.Reason == reason {
				return true, nil
			}
		}
		return false, nil
	})
}

func assertNoAllocation(t *testing.T, kube client.Client, instance *v1.AgentInstance) {
	t.Helper()
	if instance.Status.Provisioning != nil || instance.Status.SandboxUID != "" || instance.Status.PodUID != "" {
		t.Fatal("rejected configuration started allocation", instance.Name)
	}
	claims := sandbox.ExtensionList("SandboxClaim")
	if err := kube.List(t.Context(), claims, client.InNamespace(instance.Namespace)); err != nil {
		t.Fatal(err)
	}
	for _, claim := range claims.Items {
		if owner := metav1.GetControllerOf(&claim); owner != nil && owner.UID == instance.UID {
			t.Fatal("rejected configuration created a SandboxClaim", claim.GetName())
		}
	}
}

func verifyAdmissionDenial(t *testing.T, kube client.Client, instance *v1.AgentInstance) {
	t.Helper()
	ctx := t.Context()
	execution := sandbox.Object()
	if instance.Status.SandboxUID == "" {
		t.Fatal("admission rejection has no observed Sandbox")
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.SandboxName}, execution); err != nil {
		t.Fatal(err)
	}
	decoded, err := sandbox.Decode(execution)
	if err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(decoded.Status.Conditions, "Ready")
	if execution.GetUID() != instance.Status.SandboxUID || condition == nil || condition.ObservedGeneration != execution.GetGeneration() ||
		condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, "violates PodSecurity") {
		t.Fatal("diagnosis has no current native admission rejection", condition)
	}
	pods := &corev1.PodList{}
	if err := kube.List(ctx, pods, client.InNamespace(instance.Namespace)); err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods.Items {
		if owner := metav1.GetControllerOf(&pod); owner != nil && owner.UID == execution.GetUID() {
			t.Fatal("admission allowed a rejected execution Pod", pod.Name)
		}
	}
	await(t, "real Core admission Event with safe Operator feedback", func() (bool, error) {
		events := &corev1.EventList{}
		if err := kube.List(ctx, events, client.InNamespace(instance.Namespace)); err != nil {
			return false, err
		}
		found := false
		for _, event := range events.Items {
			if event.InvolvedObject.UID == execution.GetUID() && event.Reason == "SandboxPodCreateFailed" && strings.Contains(event.Message, "violates PodSecurity") {
				found = true
			}
			if event.InvolvedObject.UID == instance.UID && strings.Contains(event.Message, "security-detail-canary") {
				t.Fatal("Operator Event copied raw admission detail")
			}
		}
		return found, nil
	})
	conditions, err := json.Marshal(instance.Status.Conditions)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(conditions), "security-detail-canary") {
		t.Fatal("Instance conditions copied raw admission detail")
	}
}

func verifyRestrictedProcess(t *testing.T, kubeconfig string, instance *v1.AgentInstance) {
	t.Helper()
	output := kubectl(t, kubeconfig, "-n", instance.Namespace, "exec", instance.Status.PodName, "-c", "agent", "--", "sh", "-ec", `test "$(id -u)" = 1000
test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
awk '/^CapEff:/ {if ($2 != "0000000000000000") exit 1; caps=1}
     /^NoNewPrivs:/ {if ($2 != "1") exit 1; nnp=1}
     END {if (!caps || !nnp) exit 1}' /proc/1/status
printf restricted`)
	if output != "restricted" {
		t.Fatal("process security probe failed", output)
	}
}

func removeScenarioGroup(t *testing.T, kube client.Client, group *v1.AgentGroup) {
	t.Helper()
	ctx := t.Context()
	instances := &v1.AgentInstanceList{}
	if err := kube.List(ctx, instances, client.InNamespace(group.Status.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range instances.Items {
		if err := kube.Delete(ctx, &instances.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
	for i := range instances.Items {
		awaitDeleted(t, ctx, kube, &instances.Items[i])
	}
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}
