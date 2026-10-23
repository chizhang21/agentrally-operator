package agentinstance

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "arally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func gatewayFixture(name, namespace, protocol, hostname string) *unstructured.Unstructured {
	listener := strings.ToLower(protocol)
	condition := func(kind string) interface{} {
		return map[string]interface{}{"type": kind, "status": "True", "observedGeneration": int64(1)}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]interface{}{"name": name, "namespace": namespace, "generation": int64(1)},
		"spec": map[string]interface{}{"gatewayClassName": "test", "listeners": []interface{}{map[string]interface{}{
			"name": listener, "protocol": protocol, "port": int64(80), "hostname": hostname,
			"allowedRoutes": map[string]interface{}{"namespaces": map[string]interface{}{"from": "All"}},
		}}},
		"status": map[string]interface{}{"conditions": []interface{}{condition("Accepted"), condition("Programmed")}, "listeners": []interface{}{map[string]interface{}{
			"name": listener, "supportedKinds": []interface{}{map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute"}},
			"conditions": []interface{}{condition("Accepted"), condition("Programmed"), condition("ResolvedRefs")},
		}}},
	}}
}

func addGateway(t *testing.T, r *Reconciler, gateway *unstructured.Unstructured) {
	t.Helper()
	gv := schema.GroupVersion{Group: "gateway.networking.k8s.io", Version: "v1"}
	r.Scheme.AddKnownTypeWithName(gv.WithKind("Gateway"), &unstructured.Unstructured{})
	r.Scheme.AddKnownTypeWithName(gv.WithKind("GatewayList"), &unstructured.UnstructuredList{})
	if err := r.Client.Create(ctx, gateway); err != nil {
		t.Fatal(err)
	}
}

func setGroupInteraction(t *testing.T, r *Reconciler, w *v1.AgentInstance, defaults *v1.HTTPRouteDefaults) {
	t.Helper()
	group := &v1.AgentGroup{}
	if err := r.Get(ctx, client.ObjectKey{Name: w.Spec.AgentGroupRef.Name}, group); err != nil {
		t.Fatal(err)
	}
	group.Spec.InteractionDefaults = &v1.InteractionDefaults{HTTPRoute: defaults}
	if err := r.Update(ctx, group); err != nil {
		t.Fatal(err)
	}
}

func TestInteractionTypeOnlyUsesGroupDefaultsWithoutMutatingClaim(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	w.Spec.Interaction = &v1.InteractionEndpoint{Type: "HTTPRoute"}
	core.Spec.PodTemplate.Spec.Containers = []corev1.Container{{Name: "agent", Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9090}, {Name: "http", ContainerPort: 8000}}}}
	addGateway(t, r, gatewayFixture("public", "gateway-system", "HTTP", ""))
	setGroupInteraction(t, r, w, &v1.HTTPRouteDefaults{GatewayRef: &v1.GatewayReference{Name: "public", Namespace: "gateway-system"}, Domain: "agents.example.com"})
	before := w.DeepCopy()
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.status == nil {
		t.Fatalf("type-only endpoint: %+v %v", state, err)
	}
	if !strings.HasPrefix(state.status.URL, "http://") || !strings.HasSuffix(state.status.URL, ".agents.example.com") || state.status.Port != 8000 || state.status.GatewayRef == nil || state.status.GatewayRef.Name != "public" || state.status.GatewayRef.SectionName != "http" {
		t.Fatalf("wrong resolved endpoint: %+v", state.status)
	}
	if !reflect.DeepEqual(w, before) {
		t.Fatal("default resolution mutated Claim")
	}
	service := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}, service); err != nil || service.Spec.Ports[0].TargetPort.IntVal != 8000 {
		t.Fatalf("wrong resolved target port: %+v %v", service.Spec, err)
	}
	route := readInteractionRoute(t, r, w, "HTTPRoute")
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if parents[0].(map[string]interface{})["name"] != "public" {
		t.Fatal("Group Gateway not used")
	}
}

func TestOpenShiftTypeOnlyIgnoresHTTPDefaultsAndGatewayAPI(t *testing.T) {
	r, w, core := interactionFixture(t, "OpenShiftRoute")
	w.Spec.Interaction = &v1.InteractionEndpoint{Type: "OpenShiftRoute"}
	core.Spec.PodTemplate.Spec.Containers = []corev1.Container{{Name: "agent", Ports: []corev1.ContainerPort{{ContainerPort: 8000}}}}
	setGroupInteraction(t, r, w, &v1.HTTPRouteDefaults{GatewayRef: &v1.GatewayReference{Name: "missing", Namespace: "missing"}, Domain: "unused.example.com"})
	r.InteractionClient = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Gateway" {
			t.Fatal("OpenShift consulted HTTPRoute Gateway")
		}
		return c.Get(ctx, key, obj, opts...)
	}, List: func(ctx context.Context, c client.WithWatch, obj client.ObjectList, opts ...client.ListOption) error {
		t.Fatal("OpenShift performed Gateway discovery")
		return nil
	}})
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.status == nil || state.status.Port != 8000 || state.status.GatewayRef != nil {
		t.Fatalf("OpenShift defaults: %+v %v", state, err)
	}
	route := readInteractionRoute(t, r, w, "OpenShiftRoute")
	if _, found, _ := unstructured.NestedString(route.Object, "spec", "host"); found {
		t.Fatal("OpenShift inherited Group domain")
	}
	if tls, found, _ := unstructured.NestedMap(route.Object, "spec", "tls"); !found || tls["termination"] != "edge" {
		t.Fatal("OpenShift HTTPS default missing")
	}
}

func TestInteractionDiscoveryUsesUniqueGatewayAndRejectsAmbiguity(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	w.Spec.Interaction = &v1.InteractionEndpoint{Type: "HTTPRoute"}
	core.Spec.PodTemplate.Spec.Containers = []corev1.Container{{Name: "agent", Ports: []corev1.ContainerPort{{ContainerPort: 8000}}}}
	addGateway(t, r, gatewayFixture("public", "gateway-system", "HTTPS", "*.agents.example.com"))
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.status == nil || !strings.HasPrefix(state.status.URL, "https://") {
		t.Fatalf("unique Gateway: %+v %v", state, err)
	}
	addGateway(t, r, gatewayFixture("other", "gateway-system", "HTTPS", "*.agents.example.com"))
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition == nil || state.condition.Reason != "GatewayAmbiguous" {
		t.Fatalf("ambiguous Gateway: %+v %v", state, err)
	}
}

func TestInteractionRejectsUnknownPortWithoutCreatingAccessResources(t *testing.T) {
	for _, ports := range [][]corev1.ContainerPort{nil, {{ContainerPort: 8000}, {ContainerPort: 9000}}, {{Name: "http", ContainerPort: 8000, Protocol: corev1.ProtocolUDP}}} {
		r, w, core := interactionFixture(t, "OpenShiftRoute")
		w.Spec.Interaction = &v1.InteractionEndpoint{Type: "OpenShiftRoute"}
		core.Spec.PodTemplate.Spec.Containers = []corev1.Container{{Name: "agent", Ports: ports}}
		state, err := r.reconcileInteraction(ctx, w, core, 7)
		if err != nil || state.condition == nil || state.condition.Reason != "InteractionPortRequired" {
			t.Fatalf("ambiguous/missing HTTP port: %+v %v", state, err)
		}
		services := &corev1.ServiceList{}
		if err := r.List(ctx, services, client.InNamespace(w.Namespace)); err != nil || len(services.Items) != 0 {
			t.Fatalf("created Service before port resolution: %v", err)
		}
	}
}

func TestInteractionExplicitOverridesGroup(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	setGroupInteraction(t, r, w, &v1.HTTPRouteDefaults{GatewayRef: &v1.GatewayReference{Name: "unused"}, Domain: "unused.example.com"})
	addGateway(t, r, gatewayFixture("shared", "gateway-system", "HTTPS", "*.example.com"))
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.status == nil || state.status.URL != "https://agent.example.com" || state.status.Port != 8080 || state.status.GatewayRef.Name != "shared" {
		t.Fatalf("explicit overrides: %+v %v", state, err)
	}
}

func TestInteractionDiscoveryChecksGatewayAdmissionRules(t *testing.T) {
	tests := []struct {
		name   string
		change func(*unstructured.Unstructured)
		want   string
	}{
		{"same-namespace", func(g *unstructured.Unstructured) {
			listeners, _, _ := unstructured.NestedSlice(g.Object, "spec", "listeners")
			listeners[0].(map[string]interface{})["allowedRoutes"] = map[string]interface{}{"namespaces": map[string]interface{}{"from": "Same"}}
			_ = unstructured.SetNestedSlice(g.Object, listeners, "spec", "listeners")
		}, "GatewayUnavailable"},
		{"namespace-selector-denied", func(g *unstructured.Unstructured) {
			listeners, _, _ := unstructured.NestedSlice(g.Object, "spec", "listeners")
			listeners[0].(map[string]interface{})["allowedRoutes"] = map[string]interface{}{"namespaces": map[string]interface{}{"from": "Selector", "selector": map[string]interface{}{"matchLabels": map[string]interface{}{"ingress": "enabled"}}}}
			_ = unstructured.SetNestedSlice(g.Object, listeners, "spec", "listeners")
		}, "GatewayUnavailable"},
		{"route-kind-denied", func(g *unstructured.Unstructured) {
			listeners, _, _ := unstructured.NestedSlice(g.Object, "spec", "listeners")
			listeners[0].(map[string]interface{})["allowedRoutes"] = map[string]interface{}{"kinds": []interface{}{map[string]interface{}{"kind": "GRPCRoute"}}, "namespaces": map[string]interface{}{"from": "All"}}
			_ = unstructured.SetNestedSlice(g.Object, listeners, "spec", "listeners")
		}, "GatewayUnavailable"},
		{"stale-gateway", func(g *unstructured.Unstructured) { g.SetGeneration(2) }, "GatewayUnavailable"},
		{"no-domain", func(g *unstructured.Unstructured) {
			listeners, _, _ := unstructured.NestedSlice(g.Object, "spec", "listeners")
			delete(listeners[0].(map[string]interface{}), "hostname")
			_ = unstructured.SetNestedSlice(g.Object, listeners, "spec", "listeners")
		}, "InteractionDomainRequired"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, w, core := interactionFixture(t, "HTTPRoute")
			w.Spec.Interaction = &v1.InteractionEndpoint{Type: "HTTPRoute", Port: 8000}
			g := gatewayFixture("public", "gateway-system", "HTTP", "*.agents.example.com")
			tc.change(g)
			addGateway(t, r, g)
			state, err := r.reconcileInteraction(ctx, w, core, 7)
			if err != nil || state.condition == nil || state.condition.Reason != tc.want {
				t.Fatalf("%s: %+v %v", tc.name, state, err)
			}
		})
	}
}

func TestInteractionDiscoveryAcceptsNamespaceSelectorAndNativeDefault(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	w.Spec.Interaction = &v1.InteractionEndpoint{Type: "HTTPRoute", Port: 8000}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: w.Namespace}, ns); err != nil {
		t.Fatal(err)
	}
	ns.Labels = map[string]string{"ingress": "enabled"}
	if err := r.Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	chosen := gatewayFixture("default", "gateway-system", "HTTPS", "*.agents.example.com")
	listeners, _, _ := unstructured.NestedSlice(chosen.Object, "spec", "listeners")
	listeners[0].(map[string]interface{})["allowedRoutes"] = map[string]interface{}{"namespaces": map[string]interface{}{"from": "Selector", "selector": map[string]interface{}{"matchLabels": map[string]interface{}{"ingress": "enabled"}}}}
	_ = unstructured.SetNestedSlice(chosen.Object, listeners, "spec", "listeners")
	_ = unstructured.SetNestedField(chosen.Object, "All", "spec", "defaultScope")
	conditions, _, _ := unstructured.NestedSlice(chosen.Object, "status", "conditions")
	conditions = append(conditions, map[string]interface{}{"type": "DefaultGateway", "status": "True", "observedGeneration": int64(1)})
	_ = unstructured.SetNestedSlice(chosen.Object, conditions, "status", "conditions")
	addGateway(t, r, chosen)
	addGateway(t, r, gatewayFixture("other", "gateway-system", "HTTPS", "*.other.example.com"))
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.status == nil || state.status.GatewayRef.Name != "default" {
		t.Fatalf("native default/namespace selector: %+v %v", state, err)
	}
}

func TestInteractionGeneratedHostnamesAreUniqueAndBounded(t *testing.T) {
	seen := map[string]bool{}
	for _, identity := range []struct{ name, namespace string }{{"a-b", "arally-c"}, {"a", "arally-b-c"}, {"a.b", "arally-c"}, {strings.Repeat("a", 253), "arally-c"}, {"same", "arally-one"}, {"same", "arally-two"}} {
		w := &v1.AgentInstance{}
		w.Name = identity.name
		w.Namespace = identity.namespace
		host := interactionHostname(w, "agents.example.com")
		if seen[host] || len(strings.Split(host, ".")[0]) > 63 {
			t.Fatalf("invalid/colliding hostname: %s", host)
		}
		seen[host] = true
		if again := interactionHostname(w, "agents.example.com"); again != host {
			t.Fatal("hostname is not stable")
		}
	}
}

func TestInteractionResolvedParentAdmissionAndGroupChange(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	w.Spec.Interaction = &v1.InteractionEndpoint{Type: "HTTPRoute", Port: 8000}
	addGateway(t, r, gatewayFixture("public", "gateway-system", "HTTPS", ""))
	setGroupInteraction(t, r, w, &v1.HTTPRouteDefaults{GatewayRef: &v1.GatewayReference{Name: "public", Namespace: "gateway-system"}, Domain: "agents.example.com"})
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.status == nil {
		t.Fatalf("initial resolution: %+v %v", state, err)
	}
	previousURL := state.status.URL
	route := readInteractionRoute(t, r, w, "HTTPRoute")
	route.SetGeneration(4)
	parent, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	conditions := []interface{}{map[string]interface{}{"type": "Accepted", "status": "True", "observedGeneration": int64(4)}, map[string]interface{}{"type": "ResolvedRefs", "status": "True", "observedGeneration": int64(4)}}
	if err := unstructured.SetNestedSlice(route.Object, []interface{}{map[string]interface{}{"parentRef": parent[0], "conditions": conditions}}, "status", "parents"); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, route); err != nil {
		t.Fatal(err)
	}
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Status != "True" {
		t.Fatalf("resolved parent admission: %+v %v", state, err)
	}
	setGroupInteraction(t, r, w, &v1.HTTPRouteDefaults{GatewayRef: &v1.GatewayReference{Name: "public", Namespace: "gateway-system"}, Domain: "new.example.com"})
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.status == nil || state.status.URL == previousURL || !strings.HasSuffix(state.status.URL, ".new.example.com") {
		t.Fatalf("Group update did not update endpoint: %+v %v", state, err)
	}
	if w.Spec.Interaction.GatewayRef != nil || w.Spec.Interaction.Hostname != "" {
		t.Fatal("Group defaults persisted in Claim spec")
	}
}
