package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

var podResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

func managementFixture(t *testing.T) (*dynamicfake.FakeDynamicClient, Request) {
	t.Helper()
	owner := func(version, kind, name, uid string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: version, Kind: kind, Name: name, UID: types.UID(uid), Controller: ptr.To(true)}}
	}
	group := &v1.AgentGroup{TypeMeta: metav1.TypeMeta{APIVersion: v1.GroupVersion.String(), Kind: "AgentGroup"}, ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "group-id", ResourceVersion: "1"}}
	ns := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{
		Name: "agentrally-team", UID: "namespace-id", ResourceVersion: "1", OwnerReferences: owner(v1.GroupVersion.String(), "AgentGroup", "team", "group-id"),
		Annotations: map[string]string{"private": "must-not-leave-namespace"}, Labels: map[string]string{"private": "must-not-leave-namespace"},
	}}
	instance := &v1.AgentInstance{TypeMeta: metav1.TypeMeta{APIVersion: v1.GroupVersion.String(), Kind: "AgentInstance"}, ObjectMeta: metav1.ObjectMeta{
		Name: "agent", Namespace: ns.Name, UID: "instance-id", ResourceVersion: "1", Generation: 1,
	}, Spec: v1.AgentInstanceSpec{AgentGroupRef: v1.AgentGroupReference{Name: group.Name}, DefinitionRef: v1.AgentDefinitionReference{Name: "definition"}}, Status: v1.AgentInstanceStatus{
		Definition:   &v1.DefinitionBinding{Name: "definition", UID: "definition-id", Revision: strings.Repeat("a", 64), AgentContainer: "agent", InvocationProtocol: "HTTPJSONSSEV1"},
		Provisioning: &v1.AgentInstanceProvisioningStatus{ClaimName: "claim"},
	}}
	claim := sandbox.ExtensionObject("SandboxClaim")
	claim.SetName("claim")
	claim.SetNamespace(ns.Name)
	claim.SetUID("claim-id")
	claim.SetResourceVersion("1")
	claim.SetOwnerReferences(owner(v1.GroupVersion.String(), "AgentInstance", instance.Name, string(instance.UID)))
	claim.Object["status"] = map[string]any{"sandbox": map[string]any{"name": "sandbox"}}
	current := sandbox.Object()
	current.SetName("sandbox")
	current.SetNamespace(ns.Name)
	current.SetUID("sandbox-id")
	current.SetResourceVersion("1")
	current.SetOwnerReferences(owner(sandbox.ExtensionsGroupVersion.String(), "SandboxClaim", claim.GetName(), string(claim.GetUID())))
	current.SetAnnotations(map[string]string{"agents.x-k8s.io/pod-name": "warm-pod"})
	pod := &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{
		Name: "warm-pod", Namespace: ns.Name, UID: "pod-id", ResourceVersion: "1", OwnerReferences: owner(sandbox.GroupVersion.String(), "Sandbox", current.GetName(), string(current.GetUID())),
	}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sidecar", Image: "example/sidecar"}, {Name: "agent", Image: "example/agent"}}}, Status: corev1.PodStatus{
		Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", ContainerID: "containerd://agent-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Unix(100, 0))}}}},
	}}
	var objects []runtime.Object
	for _, object := range []runtime.Object{group, ns, instance, claim, pod} {
		fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, &unstructured.Unstructured{Object: fields})
	}
	kube := fakeKube(objects...)
	// The fake tracker's automatic pluralization guesses "sandboxs".
	if err := kube.Tracker().Create(sandbox.GroupVersion.WithResource("sandboxes"), current, ns.Name); err != nil {
		t.Fatal(err)
	}
	return kube, Request{ID: "read", Version: "v2", Method: http.MethodGet, Resource: "instanceobservations", Namespace: ns.Name, Name: instance.Name,
		Scope: &RequestScope{GroupName: group.Name, GroupUID: string(group.UID), NamespaceUID: string(ns.UID)}}
}

func TestCapabilitiesAndUnsupportedIntentsNeverTouchKubernetes(t *testing.T) {
	kube := fakeKube()
	response := (Executor{Client: kube}).Execute(context.Background(), Request{Method: http.MethodGet, Resource: "connectorcapabilities"})
	var capabilities capabilityDocument
	if response.Status != 200 || json.Unmarshal(response.Body, &capabilities) != nil || len(capabilities.Management) != 2 || capabilities.Invocation || capabilities.ExclusiveHandoff || capabilities.ObservedOperatingMode || capabilities.ActivityLease || capabilities.ProcessIdentity || capabilities.ScopedNamespaceCreate {
		t.Fatalf("capabilities overstate support: %s", response.Body)
	}
	for _, req := range []Request{
		{Version: "v2", Method: http.MethodPost, Resource: "exclusivehandoff"},
		{Version: "v9", Method: http.MethodGet, Resource: "agentgroups"},
		{Version: "v2", Method: http.MethodPut, Resource: "agentinstances", Intent: &ControlIntent{Kind: "Observed", ExpectedExecutionID: "old-execution", ObservedAt: "2026-10-28T00:00:00Z"}},
		{Version: "v2", Method: http.MethodGet, Resource: "instanceobservations", ObservationScope: "Process"},
		{Version: "v2", Method: http.MethodPost, Resource: "agentinstances", Namespace: "team"},
	} {
		if got := (Executor{Client: kube}).Execute(context.Background(), req); got.Status != 501 {
			t.Fatalf("unsupported operation was accepted: %+v: %s", req, got.Body)
		}
	}
	if len(kube.Actions()) != 0 {
		t.Fatal("unsupported intent or discovery touched Kubernetes")
	}
}

func TestNamespaceDiscoveryAndScopedManagement(t *testing.T) {
	kube, req := managementFixture(t)
	executor := Executor{Client: kube}
	req.Resource, req.Name, req.Namespace, req.Scope.NamespaceUID = "namespaces", "", "", ""
	response := executor.Execute(context.Background(), req)
	if response.Status != 200 || !strings.Contains(string(response.Body), "namespace-id") || strings.Contains(string(response.Body), "must-not-leave-namespace") {
		t.Fatalf("first Namespace discovery failed or leaked metadata: %s", response.Body)
	}
	req.Scope.NamespaceUID = "replaced-namespace"
	if got := executor.Execute(context.Background(), req); got.Status != 403 {
		t.Fatalf("namespace replacement was accepted: %s", got.Body)
	}
	_, req = managementFixture(t)
	req.Resource, req.Name, req.Namespace, req.Scope.NamespaceUID = "agentgroups", "another-group", "", ""
	if got := executor.Execute(context.Background(), req); got.Status != 403 {
		t.Fatalf("Group B was addressed using Group A scope: %s", got.Body)
	}
	_, req = managementFixture(t)
	req.Resource = "agentinstances"
	object, err := kube.Resource(workloadGVR).Namespace(req.Namespace).Get(context.Background(), req.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(object.Object, "Suspended", "spec", "operatingMode"); err != nil {
		t.Fatal(err)
	}
	req.Method, req.Body, req.Intent = http.MethodPut, mustJSON(object), &ControlIntent{Kind: "Explicit"}
	response = executor.Execute(context.Background(), req)
	if response.Status != 200 {
		t.Fatalf("scoped explicit update failed: %s", response.Body)
	}
	var update interface{ GetUpdateOptions() metav1.UpdateOptions }
	for _, action := range kube.Actions() {
		if value, ok := action.(interface{ GetUpdateOptions() metav1.UpdateOptions }); ok {
			update = value
		}
	}
	if update == nil || update.GetUpdateOptions().FieldValidation != metav1.FieldValidationStrict {
		t.Fatal("native update did not request strict schema validation")
	}
	object.SetUID("wrong-instance")
	req.Body = mustJSON(object)
	if got := executor.Execute(context.Background(), req); got.Status != 403 {
		t.Fatalf("same-name different UID update accepted: %s", got.Body)
	}
}

func readObservation(t *testing.T, kube *dynamicfake.FakeDynamicClient, req Request) InstanceObservation {
	t.Helper()
	response := (Executor{Client: kube}).Execute(context.Background(), req)
	var observation InstanceObservation
	if response.Status != 200 || json.Unmarshal(response.Body, &observation) != nil {
		t.Fatalf("observation failed: %s", response.Body)
	}
	return observation
}

func TestContainerRestartAndIncompleteBindingObservations(t *testing.T) {
	kube, req := managementFixture(t)
	first := readObservation(t, kube, req)
	if first.Execution == nil || first.State != "Active" || first.Execution.ContainerName != "agent" || first.ExecutionRelease != "Unknown" || first.ProcessIdentityVerified || first.Invocation.Supported {
		t.Fatalf("incorrect initial observation: %+v", first)
	}
	pod, _ := kube.Resource(podResource).Namespace(req.Namespace).Get(context.Background(), "warm-pod", metav1.GetOptions{})
	statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
	statuses[0].(map[string]any)["restartCount"] = int64(1)
	statuses[0].(map[string]any)["containerID"] = "containerd://agent-2"
	_ = unstructured.SetNestedSlice(pod.Object, statuses, "status", "containerStatuses")
	pod.SetResourceVersion("2")
	if _, err := kube.Resource(podResource).Namespace(req.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	second := readObservation(t, kube, req)
	if second.Execution == nil || second.Execution.ID == first.Execution.ID || second.InstanceID != first.InstanceID || second.InstanceUID != first.InstanceUID {
		t.Fatal("same-Pod container restart did not invalidate execution identity")
	}
	object, _ := kube.Resource(workloadGVR).Namespace(req.Namespace).Get(context.Background(), req.Name, metav1.GetOptions{})
	unstructured.RemoveNestedField(object.Object, "status", "definition", "agentContainer")
	if _, err := kube.Resource(workloadGVR).Namespace(req.Namespace).Update(context.Background(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	legacy := readObservation(t, kube, req)
	if legacy.Execution != nil || legacy.State != "" || legacy.Reason != "BindingObservationUnavailable" {
		t.Fatal("legacy binding guessed a container identity")
	}
}

func TestObservationRejectsReplacementDuringRead(t *testing.T) {
	kube, req := managementFixture(t)
	reads := 0
	kube.PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads == 2 {
			object, err := kube.Tracker().Get(podResource, req.Namespace, "warm-pod")
			if err != nil {
				return true, nil, err
			}
			pod := object.(*unstructured.Unstructured)
			pod.SetUID("replacement-pod")
			if err := kube.Tracker().Update(podResource, pod, req.Namespace); err != nil {
				return true, nil, err
			}
		}
		return false, nil, nil
	})
	if got := (Executor{Client: kube}).Execute(context.Background(), req); got.Status != 409 {
		t.Fatalf("replacement during observation was hidden: %s", got.Body)
	}
}

func TestObservationLifecycleDoesNotPromiseExecutionRelease(t *testing.T) {
	for _, scenario := range []struct{ name, state, reason string }{
		{"ready", "Active", "ContainerObserved"},
		{"missing-container-id", "", "ContainerObservationUnavailable"},
		{"terminal", "Terminated", "ContainerTerminationObserved"},
		{"suspending", "", "Stopping"},
		{"suspended", "Idle", "SuspendedResourcesObserved"},
		{"stale-suspension", "", "Stopping"},
		{"expired", "", "ExecutionResourcesRemoved"},
		{"restart-with-newer-spec", "", "Stopping"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			kube, req := managementFixture(t)
			instance, _ := kube.Resource(workloadGVR).Namespace(req.Namespace).Get(context.Background(), req.Name, metav1.GetOptions{})
			pod, _ := kube.Resource(podResource).Namespace(req.Namespace).Get(context.Background(), "warm-pod", metav1.GetOptions{})
			switch scenario.name {
			case "restart-with-newer-spec":
				_ = unstructured.SetNestedSlice(instance.Object, []any{map[string]any{"type": "Restarting", "status": "True", "observedGeneration": instance.GetGeneration() - 1}}, "status", "conditions")
			case "missing-container-id":
				_ = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"name": "agent"}}, "status", "containerStatuses")
			case "terminal":
				_ = unstructured.SetNestedField(pod.Object, "Succeeded", "status", "phase")
				_ = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"name": "agent", "state": map[string]any{"terminated": map[string]any{"exitCode": int64(0)}}}}, "status", "containerStatuses")
			case "suspending", "suspended", "stale-suspension":
				_ = unstructured.SetNestedField(instance.Object, "Suspended", "spec", "operatingMode")
				generation := instance.GetGeneration()
				if scenario.name == "stale-suspension" {
					generation--
				}
				_ = unstructured.SetNestedSlice(instance.Object, []any{map[string]any{"type": "Suspended", "status": "True", "observedGeneration": generation}}, "status", "conditions")
			case "expired":
				_ = unstructured.SetNestedField(instance.Object, "2026-10-28T00:00:00Z", "status", "lifecycle", "expiredAt")
				_ = unstructured.SetNestedSlice(instance.Object, []any{map[string]any{"type": "CleanupReady", "status": "True", "observedGeneration": instance.GetGeneration()}}, "status", "conditions")
			}
			if err := kube.Tracker().Update(workloadGVR, instance, req.Namespace); err != nil {
				t.Fatal(err)
			}
			if err := kube.Tracker().Update(podResource, pod, req.Namespace); err != nil {
				t.Fatal(err)
			}
			if scenario.name == "suspended" || scenario.name == "stale-suspension" || scenario.name == "expired" {
				if err := kube.Tracker().Delete(podResource, req.Namespace, pod.GetName()); err != nil {
					t.Fatal(err)
				}
			}
			got := readObservation(t, kube, req)
			if got.State != scenario.state || got.Reason != scenario.reason || got.StateEvidence != "KubernetesObservation" || got.ExecutionRelease != "Unknown" {
				t.Fatalf("state incorrectly implies lifecycle or release guarantees: %+v", got)
			}
		})
	}
}

func TestObservationBindsRecreatedInstancesAndRejectsForeignOwners(t *testing.T) {
	kube, req := managementFixture(t)
	before := readObservation(t, kube, req)
	instance, _ := kube.Resource(workloadGVR).Namespace(req.Namespace).Get(context.Background(), req.Name, metav1.GetOptions{})
	instance.SetUID("replacement-instance")
	if err := kube.Tracker().Update(workloadGVR, instance, req.Namespace); err != nil {
		t.Fatal(err)
	}
	// The old claim is still owned by the deleted Instance UID.
	if got := (Executor{Client: kube}).Execute(context.Background(), req); got.Status != 403 {
		t.Fatalf("new instance consumed an old instance's execution: %s", got.Body)
	}
	unstructured.RemoveNestedField(instance.Object, "status", "provisioning")
	if err := kube.Tracker().Update(workloadGVR, instance, req.Namespace); err != nil {
		t.Fatal(err)
	}
	after := readObservation(t, kube, req)
	if after.InstanceID != before.InstanceID || after.GroupUID != before.GroupUID || after.InstanceUID == before.InstanceUID || after.Execution != nil || after.State != "" {
		t.Fatal("recreation confused stable identity with CR or execution identity")
	}
}

func TestRetiredActivityFieldsAreRejectedBeforeNativeWrites(t *testing.T) {
	for _, version := range []string{"", "v1", "v2"} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			for _, field := range []string{"idleTimeoutSeconds", "activityLeaseRef"} {
				kube, req := managementFixture(t)
				object, _ := kube.Resource(workloadGVR).Namespace(req.Namespace).Get(context.Background(), req.Name, metav1.GetOptions{})
				_ = unstructured.SetNestedField(object.Object, map[string]any{field: "obsolete"}, "spec", "lifecycle")
				if method == http.MethodPost {
					object.SetUID("")
					object.SetResourceVersion("")
					req.Name = ""
				}
				req.Version, req.Resource, req.Method, req.Body = version, "agentinstances", method, mustJSON(object)
				if version != "v2" {
					req.Scope = nil
				}
				kube.ClearActions()
				result := (Executor{Client: kube}).Execute(context.Background(), req)
				if result.Status != 400 || !strings.Contains(string(result.Body), "RemovedActivityContract") {
					t.Fatalf("%s %s retired field was silently accepted: %s", version, method, result.Body)
				}
				for _, action := range kube.Actions() {
					if action.GetVerb() != "get" {
						t.Fatal("retired contract produced a write")
					}
				}
			}
		}
	}
}
