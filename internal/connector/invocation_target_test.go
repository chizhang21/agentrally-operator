package connector

import (
	"context"
	"strconv"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var testInvocationPeer = controller.InvocationPeer{Namespace: "operator-system", ReleaseName: "operator"}
var policyResource = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}
var endpointResource = schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}

func invocationFixture(t *testing.T, ip string, port int32) (*dynamicfake.FakeDynamicClient, InvocationCommand) {
	t.Helper()
	kube, observation := managementFixture(t)
	ctx := context.Background()
	update := func(resource schema.GroupVersionResource, namespace, name string, edit func(*unstructured.Unstructured)) {
		t.Helper()
		object, err := kube.Resource(resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		edit(object)
		if _, err := kube.Resource(resource).Namespace(namespace).Update(ctx, object, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	group := &v1.AgentGroup{}
	update(groupGVR, "", "team", func(object *unstructured.Unstructured) {
		if err := unstructured.SetNestedMap(object.Object, map[string]any{"mode": "ConnectorOnly"}, "spec", "invocationAccess"); err != nil {
			t.Fatal(err)
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, group); err != nil {
			t.Fatal(err)
		}
	})
	declared := &v1.AgentDefinition{TypeMeta: metav1.TypeMeta{APIVersion: v1.GroupVersion.String(), Kind: "AgentDefinition"}, ObjectMeta: metav1.ObjectMeta{
		Name: "definition", Namespace: observation.Namespace, UID: "definition-id"}, Spec: v1.AgentDefinitionSpec{AgentGroupRef: v1.AgentGroupReference{Name: "team"}, Template: "generic",
		Container: &v1.AgentContainerSpec{Image: "example/agent", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}}}, InvocationProtocol: "HTTPJSONSSEV1"}}
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	typed := fake.NewClientBuilder().WithScheme(scheme).WithObjects(declared).Build()
	revision, err := definition.Publish(ctx, typed, typed, scheme, declared)
	if err != nil {
		t.Fatal(err)
	}
	put := func(resource schema.GroupVersionResource, object runtime.Object) {
		t.Helper()
		fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
		if err != nil {
			t.Fatal(err)
		}
		value := &unstructured.Unstructured{Object: fields}
		if err := kube.Tracker().Create(resource, value, value.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	put(definitionGVR, declared)
	put(sandbox.ExtensionsGroupVersion.WithResource("sandboxtemplates"), revision.Template)
	update(workloadGVR, observation.Namespace, "agent", func(object *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(object.Object, revision.ID, "status", "definition", "revision"); err != nil {
			t.Fatal(err)
		}
	})
	update(podResource, observation.Namespace, "warm-pod", func(object *unstructured.Unstructured) {
		var pod corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &pod); err != nil {
			t.Fatal(err)
		}
		pod.Labels = map[string]string{"sandbox": "current"}
		pod.Status.PodIP = ip
		pod.Status.PodIPs = []corev1.PodIP{{IP: ip}}
		pod.Spec.Containers[1].Ports = declared.Spec.Container.Ports
		object.Object, _ = runtime.DefaultUnstructuredConverter.ToUnstructured(&pod)
	})
	owner := func(version, kind, name, uid string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: version, Kind: kind, Name: name, UID: types.UID(uid), Controller: ptr.To(true)}}
	}
	policy, err := controller.GroupNetworkPolicy(group, testInvocationPeer)
	if err != nil {
		t.Fatal(err)
	}
	put(policyResource, &networkingv1.NetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}, ObjectMeta: metav1.ObjectMeta{
		Name: controller.GroupNetworkPolicyName, Namespace: observation.Namespace, OwnerReferences: owner(v1.GroupVersion.String(), "AgentGroup", "team", "group-id")}, Spec: *policy})
	update(sandbox.GroupVersion.WithResource("sandboxes"), observation.Namespace, "sandbox", func(object *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(object.Object, true, "spec", "service"); err != nil {
			t.Fatal(err)
		}
	})
	put(schema.GroupVersionResource{Version: "v1", Resource: "services"}, &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{
		Name: "sandbox", Namespace: observation.Namespace, UID: "service-id", OwnerReferences: owner(sandbox.GroupVersion.String(), "Sandbox", "sandbox", "sandbox-id")}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: map[string]string{"sandbox": "current"}}})
	put(endpointResource, &discoveryv1.EndpointSlice{TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSlice"}, ObjectMeta: metav1.ObjectMeta{
		Name: "sandbox-slice", Namespace: observation.Namespace, Labels: map[string]string{discoveryv1.LabelServiceName: "sandbox"}, OwnerReferences: owner("v1", "Service", "sandbox", "service-id")},
		AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Port: ptr.To(port), Protocol: ptr.To(corev1.ProtocolTCP)}}, Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "warm-pod", Namespace: observation.Namespace, UID: "pod-id"}}}})
	c := invocationCommand()
	c.InvocationIdentity = InvocationIdentity{Scope: *observation.Scope, InstanceID: "agent", InstanceUID: "instance-id", ExecutionID: readObservation(t, kube, observation).Execution.ID}
	return kube, c
}

func TestInvocationTargetVerifiesPublishedIdentityAndEndpoint(t *testing.T) {
	for _, scenario := range []string{"valid", "stale-execution", "forged-endpoint", "changed-template", "extra-ingress", "interaction", "host-network", "missing-service", "service-disabled"} {
		t.Run(scenario, func(t *testing.T) {
			kube, c := invocationFixture(t, "10.1.2.3", 8080)
			ctx := context.Background()
			ns := "agentrally-team"
			switch scenario {
			case "stale-execution":
				c.ExecutionID = "old"
			case "extra-ingress":
				object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "extra", "namespace": ns}, "spec": map[string]any{"podSelector": map[string]any{}, "ingress": []any{map[string]any{}}}}}
				if _, err := kube.Resource(policyResource).Namespace(ns).Create(ctx, object, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			default:
				resource, name := podResource, "warm-pod"
				switch scenario {
				case "missing-service", "service-disabled":
					resource, name = sandbox.GroupVersion.WithResource("sandboxes"), "sandbox"
				case "forged-endpoint":
					resource, name = endpointResource, "sandbox-slice"
				case "changed-template":
					instance, _ := kube.Resource(workloadGVR).Namespace(ns).Get(ctx, "agent", metav1.GetOptions{})
					rev, _, _ := unstructured.NestedString(instance.Object, "status", "definition", "revision")
					resource, name = sandbox.ExtensionsGroupVersion.WithResource("sandboxtemplates"), definition.Name("definition-id", rev)
				case "interaction":
					resource, name = workloadGVR, "agent"
				}
				object, err := kube.Resource(resource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "forged-endpoint":
					object.Object["endpoints"].([]any)[0].(map[string]any)["targetRef"].(map[string]any)["uid"] = "old-pod"
				case "changed-template":
					object.SetAnnotations(map[string]string{definition.SpecAnnotation: "{}"})
				case "interaction":
					if err := unstructured.SetNestedMap(object.Object, map[string]any{"type": "Ingress"}, "spec", "interaction"); err != nil {
						t.Fatal(err)
					}
				case "missing-service":
					unstructured.RemoveNestedField(object.Object, "spec", "service")
				case "service-disabled":
					if err := unstructured.SetNestedField(object.Object, false, "spec", "service"); err != nil {
						t.Fatal(err)
					}
				case "host-network":
					if err := unstructured.SetNestedField(object.Object, true, "spec", "hostNetwork"); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := kube.Resource(resource).Namespace(ns).Update(ctx, object, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			target, err := (Executor{Client: kube}).resolveInvocation(ctx, c.InvocationIdentity, testInvocationPeer)
			if scenario == "valid" {
				if err != nil || target.Address != "10.1.2.3:"+strconv.Itoa(8080) || !strings.HasPrefix(target.Host, "sandbox.") {
					t.Fatal(target, err)
				}
			} else if scenario == "service-disabled" {
				if err != nil || target.Host != target.Address {
					t.Fatal(target, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe target accepted: %+v", target)
			}
			for _, action := range kube.Actions() {
				if action.GetVerb() == "delete" || action.GetVerb() == "patch" {
					t.Fatal("resolver mutated resources")
				}
			}
		})
	}
}
