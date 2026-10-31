package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
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
	"k8s.io/client-go/rest"
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
	return kube, Request{ID: "read", Method: http.MethodGet, Resource: "instanceobservations", Namespace: ns.Name, Name: instance.Name,
		Scope: &RequestScope{GroupName: group.Name, GroupUID: string(group.UID), NamespaceUID: string(ns.UID)}}
}

func TestSharedManagementContract(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/connector/management.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Capabilities       map[string]any    `json:"capabilities"`
		Unsupported        []Request         `json:"unsupported"`
		Create             Request           `json:"create"`
		SecretMetadata     map[string]any    `json:"secretMetadata"`
		CreateConfigMap    Request           `json:"createConfigMap"`
		ConfigMapMetadata  map[string]any    `json:"configMapMetadata"`
		InstanceDiagnostic map[string]any    `json:"instanceDiagnostic"`
		DeleteSecret       Request           `json:"deleteSecret"`
		DeleteConfigMap    Request           `json:"deleteConfigMap"`
		DeleteStatus       map[string]any    `json:"deleteStatus"`
		RejectedEnvelopes  []json.RawMessage `json:"rejectedEnvelopes"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Capabilities == nil || len(fixture.Unsupported) == 0 {
		t.Fatal("management fixture has no capability or rejection contract")
	}
	kube := fakeKube()
	for _, preview := range []bool{false, true} {
		response := (Executor{Client: kube, InvocationPreview: preview}).Execute(context.Background(), Request{Method: http.MethodGet, Resource: "connectorcapabilities"})
		var capabilities map[string]any
		fixture.Capabilities["invocationPreview"] = preview
		if response.Status != 200 || json.Unmarshal(response.Body, &capabilities) != nil || !reflect.DeepEqual(capabilities, fixture.Capabilities) {
			t.Fatalf("management capability contract differs from shared fixture (preview=%t): %s", preview, response.Body)
		}
	}
	for _, req := range fixture.Unsupported {
		if got := (Executor{Client: kube}).Execute(context.Background(), req); got.Status != 501 {
			t.Fatalf("unsupported operation was accepted: %+v: %s", req, got.Body)
		}
	}
	if len(kube.Actions()) != 0 {
		t.Fatal("unsupported intent or discovery touched Kubernetes")
	}
	for _, body := range fixture.RejectedEnvelopes {
		client, err := NewClient(testConfig(t), Executor{Client: kube})
		if err != nil {
			t.Fatal(err)
		}
		client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return remoteResponse(r, http.StatusOK, string(body)), nil
		})
		if _, err := client.poll(t.Context()); err == nil {
			t.Fatal("shared fixture's rejected envelope was accepted")
		}
	}
	if len(fixture.RejectedEnvelopes) == 0 || fixture.SecretMetadata == nil || fixture.DeleteSecret.ID == "" || fixture.DeleteStatus == nil {
		t.Fatal("shared management fixture is incomplete")
	}
	kube, baseRequest := managementFixture(t)
	observationResponse := (Executor{Client: kube}).Execute(t.Context(), baseRequest)
	var observed struct {
		Diagnostic map[string]any `json:"diagnostic"`
	}
	if observationResponse.Status != http.StatusOK || json.Unmarshal(observationResponse.Body, &observed) != nil || fixture.InstanceDiagnostic == nil || !reflect.DeepEqual(observed.Diagnostic, fixture.InstanceDiagnostic) {
		t.Fatalf("shared instance diagnostic differs from implementation: %s", observationResponse.Body)
	}
	if response := (Executor{Client: kube}).Execute(t.Context(), fixture.Create); response.Status != http.StatusCreated {
		t.Fatalf("shared create contract was rejected: %s", response.Body)
	}
	var expected metav1.PartialObjectMetadata
	if json.Unmarshal(mustJSON(fixture.SecretMetadata), &expected) != nil || expected.UID == "" || expected.ResourceVersion == "" {
		t.Fatal("shared Secret metadata fixture has no native identity")
	}
	assertCreatedReference := func(request Request, expectedBody map[string]any) {
		t.Helper()
		var identity metav1.PartialObjectMetadata
		if json.Unmarshal(mustJSON(expectedBody), &identity) != nil || identity.UID == "" || identity.ResourceVersion == "" {
			t.Fatal("shared reference fixture has no native identity")
		}
		kube.PrependReactor("create", request.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
			object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
			object.SetUID(identity.UID)
			object.SetResourceVersion(identity.ResourceVersion)
			annotations := object.GetAnnotations()
			annotations["private"] = "fixture-private-marker"
			object.SetAnnotations(annotations)
			object.SetLabels(map[string]string{"private": "fixture-private-marker"})
			return true, object, nil
		})
		response := (Executor{Client: kube}).Execute(t.Context(), request)
		var actual map[string]any
		if response.Status != http.StatusCreated || json.Unmarshal(response.Body, &actual) != nil || !reflect.DeepEqual(actual, expectedBody) {
			t.Fatalf("shared reference create contract differs from implementation: %s", response.Body)
		}
	}
	request := Request{ID: expected.Annotations[operationAnnotation], Method: http.MethodPost, Resource: "secrets", Namespace: expected.Namespace, Scope: baseRequest.Scope,
		Body: mustJSON(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": expected.Name, "namespace": expected.Namespace}, "stringData": map[string]any{"key": "fixture-private-marker"}})}
	assertCreatedReference(request, fixture.SecretMetadata)
	assertCreatedReference(fixture.CreateConfigMap, fixture.ConfigMapMetadata)
	for _, scenario := range []struct {
		request  Request
		metadata map[string]any
	}{{fixture.DeleteSecret, fixture.SecretMetadata}, {fixture.DeleteConfigMap, fixture.ConfigMapMetadata}} {
		metadata, err := NewMetadataClient(&rest.Config{Host: "https://kube.example", Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response := remoteResponse(r, http.StatusOK, string(mustJSON(scenario.metadata)))
			response.Header.Set("Content-Type", "application/json")
			return response, nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		kube.PrependReactor("delete", scenario.request.Resource, func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
		response := (Executor{Client: kube, Metadata: metadata}).Execute(t.Context(), scenario.request)
		var actual map[string]any
		if response.Status != http.StatusAccepted || json.Unmarshal(response.Body, &actual) != nil || !reflect.DeepEqual(actual, fixture.DeleteStatus) {
			t.Fatalf("shared reference delete contract differs from implementation: %s", response.Body)
		}
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
	req.Method, req.Body = http.MethodPut, mustJSON(object)
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
	incomplete := readObservation(t, kube, req)
	if incomplete.Execution != nil || incomplete.State != "" || incomplete.Reason != "BindingObservationUnavailable" {
		t.Fatal("incomplete binding guessed a container identity")
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
