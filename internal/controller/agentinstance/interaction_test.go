package agentinstance

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func interactionFixture(t *testing.T, kind string) (*Reconciler, *v1.AgentInstance, *sandbox.Sandbox) {
	t.Helper()
	group, ns, definition, w, source := testutil.Fixture()
	w.Spec.Interaction = &v1.InteractionEndpoint{Type: kind, Port: 8080, Hostname: "agent.example.com", Scheme: "HTTPS"}
	if kind == "HTTPRoute" {
		w.Spec.Interaction.GatewayRef = &v1.GatewayReference{Name: "shared", Namespace: "gateway-system", SectionName: "https"}
	}
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
	core := &sandbox.Sandbox{Status: sandbox.Status{LabelSelector: "agents.x-k8s.io/sandbox-name-hash=core-identity", Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 7}}}}
	return &Reconciler{Client: c, Scheme: scheme}, w, core
}

func readInteractionRoute(t *testing.T, r *Reconciler, w *v1.AgentInstance, kind string) *unstructured.Unstructured {
	t.Helper()
	u := interactionRoute(kind)
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func acceptHTTPRoute(t *testing.T, r *Reconciler, w *v1.AgentInstance, generation int64) {
	t.Helper()
	u := readInteractionRoute(t, r, w, "HTTPRoute")
	u.SetGeneration(4)
	parent := map[string]interface{}{"name": "shared", "namespace": "gateway-system", "sectionName": "https"}
	conditions := []interface{}{map[string]interface{}{"type": "Accepted", "status": "True", "observedGeneration": generation}, map[string]interface{}{"type": "ResolvedRefs", "status": "True", "observedGeneration": generation}}
	if err := unstructured.SetNestedSlice(u.Object, []interface{}{map[string]interface{}{"parentRef": parent, "conditions": conditions}}, "status", "parents"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
}

func TestInteractionDefaultCreatesNothing(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	w.Spec.Interaction = nil
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		t.Fatalf("created %T without interaction configuration", o)
		return nil
	}})
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.status != nil || state.condition != nil {
		t.Fatalf("default endpoint: %+v %v", state, err)
	}
}

func TestHTTPInteractionOwnsDedicatedServiceAndWaitsForAdmission(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil {
		t.Fatal(err)
	}
	if state.condition.Status != metav1.ConditionFalse || state.status.URL != "https://agent.example.com" {
		t.Fatalf("premature admission/address: %+v", state)
	}
	service := &corev1.Service{}
	key := client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}
	if err := r.Client.Get(ctx, key, service); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(service.Spec.Selector, map[string]string{"agents.x-k8s.io/sandbox-name-hash": "core-identity"}) || service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.Ports[0].Port != 80 || service.Spec.Ports[0].TargetPort.IntVal != 8080 {
		t.Fatalf("wrong access Service: %+v", service.Spec)
	}
	if err := controller.CheckOwner(service, w); err != nil {
		t.Fatal(err)
	}
	u := readInteractionRoute(t, r, w, "HTTPRoute")
	if err := controller.CheckOwner(u, w); err != nil {
		t.Fatal(err)
	}
	acceptHTTPRoute(t, r, w, 3)
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Status == metav1.ConditionTrue {
		t.Fatal("stale route admission was accepted", err)
	}
	acceptHTTPRoute(t, r, w, 4)
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Status != metav1.ConditionTrue {
		t.Fatalf("current acceptance: %+v %v", state, err)
	}
	core.Status.Conditions[0].ObservedGeneration = 6
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Status == metav1.ConditionTrue {
		t.Fatal("stale Core readiness was accepted", err)
	}
}

func TestInteractionPreservesClusterIPAndCoreService(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	coreService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: w.Namespace}, Spec: corev1.ServiceSpec{ClusterIP: "None", Selector: map[string]string{"core": "owned"}}}
	if err := r.Client.Create(ctx, coreService); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
		t.Fatal(err)
	}
	service := &corev1.Service{}
	key := client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}
	if err := r.Client.Get(ctx, key, service); err != nil {
		t.Fatal(err)
	}
	service.Spec.ClusterIP = "10.1.2.3"
	if err := r.Client.Update(ctx, service); err != nil {
		t.Fatal(err)
	}
	w.Spec.Interaction.Port = 9090
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(ctx, key, service); err != nil {
		t.Fatal(err)
	}
	if service.Spec.ClusterIP != "10.1.2.3" || service.Spec.Ports[0].TargetPort.IntVal != 9090 {
		t.Fatal("access Service update lost allocation or port")
	}
	untouched := &corev1.Service{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(coreService), untouched); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(untouched.Spec, coreService.Spec) {
		t.Fatal("Arally modified Core's Service")
	}
}

func TestOpenShiftInteractionPreservesAssignedHostAndRemovesTLS(t *testing.T) {
	r, w, core := interactionFixture(t, "OpenShiftRoute")
	w.Spec.Interaction.Hostname = ""
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil {
		t.Fatal(err)
	}
	if state.condition.Status == metav1.ConditionTrue {
		t.Fatal("route ready before admission")
	}
	u := readInteractionRoute(t, r, w, "OpenShiftRoute")
	if err := unstructured.SetNestedField(u.Object, "assigned.apps.example.com", "spec", "host"); err != nil {
		t.Fatal(err)
	}
	ingress := []interface{}{map[string]interface{}{"host": "assigned.apps.example.com", "conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "True"}}}}
	if err := unstructured.SetNestedSlice(u.Object, ingress, "status", "ingress"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Status != metav1.ConditionTrue || state.status.URL != "https://assigned.apps.example.com" {
		t.Fatalf("assigned host: %+v %v", state, err)
	}
	w.Spec.Interaction.Scheme = "HTTP"
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
		t.Fatal(err)
	}
	u = readInteractionRoute(t, r, w, "OpenShiftRoute")
	if _, found, _ := unstructured.NestedMap(u.Object, "spec", "tls"); found {
		t.Fatal("HTTP route retains stale TLS")
	}
}

func TestInteractionSwitchDisableAndForeignOwnership(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil {
		t.Fatal(err)
	}
	w.Status.Interaction = state.status
	w.Spec.Interaction.Type = "OpenShiftRoute"
	w.Spec.Interaction.GatewayRef = nil
	for range 3 {
		if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}, interactionRoute("HTTPRoute")); !apierrors.IsNotFound(err) {
		t.Fatal("old route remains", err)
	}
	readInteractionRoute(t, r, w, "OpenShiftRoute")
	w.Spec.Interaction = nil
	for range 3 {
		if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatal("disabled access Service remains", err)
	}
	r, w, core = interactionFixture(t, "HTTPRoute")
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: interactionName(w), Namespace: w.Namespace}}
	if err := r.Client.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err == nil {
		t.Fatal("adopted foreign Service")
	}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil || len(foreign.OwnerReferences) > 0 {
		t.Fatal("foreign Service modified", err)
	}
}

func TestInteractionMissingAPIAndEmptySelector(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	r.DirectClient = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(_ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute"}}
	}})
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Reason != "APIUnavailable" {
		t.Fatalf("missing optional API: %+v %v", state, err)
	}
	r.DirectClient = nil
	core.Status.LabelSelector = ""
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Status == metav1.ConditionTrue {
		t.Fatalf("empty selector: %+v %v", state, err)
	}
	service := &corev1.Service{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}, service); err == nil && len(service.Spec.Selector) == 0 {
		t.Fatal("created unscoped access Service")
	}
}

func TestInteractionPublicReconcileAndDeletion(t *testing.T) {
	r, w, _ := interactionFixture(t, "OpenShiftRoute")
	reconcileWithUpstreamSimulation(t, r, w)
	pod := ownedExecutionPod(t, r.Client, w)
	sb := getSandbox(t, r.Client, w)
	status := map[string]interface{}{"selector": "agents.x-k8s.io/sandbox-name-hash=core-identity", "conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "observedGeneration": sb.GetGeneration(), "reason": "DependenciesReady", "message": "ready"}}}
	if err := unstructured.SetNestedMap(sb.Object, status, "status"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	u := readInteractionRoute(t, r, w, "OpenShiftRoute")
	if err := unstructured.SetNestedSlice(u.Object, []interface{}{map[string]interface{}{"host": "agent.example.com", "conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "True"}}}}, "status", "ingress"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	current := getAgentInstance(t, r.Client, w)
	testutil.Condition(t, current.Status.Conditions, "Ready", metav1.ConditionTrue)
	testutil.Condition(t, current.Status.Conditions, "InteractionReady", metav1.ConditionTrue)
	if current.Status.Interaction == nil || current.Status.Interaction.URL != "https://agent.example.com" {
		t.Fatal("public status does not report the interaction URL")
	}
	// Route admission alone must not keep the endpoint ready when the owned Pod
	// is failing and Core has not yet refreshed its Ready condition.
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}
	if err := r.Client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	current = getAgentInstance(t, r.Client, w)
	assertReason(t, current, "Ready", "CrashLoopBackOff")
	assertReason(t, current, "InteractionReady", "ExecutionNotReady")
	if current.Status.Interaction == nil || current.Status.Interaction.URL != "https://agent.example.com" {
		t.Fatal("diagnostic removed the configured interaction URL")
	}
	if err := r.Client.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		reconcileWithUpstreamSimulation(t, r, w)
	}
	for _, object := range []client.Object{&v1.AgentInstance{}, sandbox.Object(), &corev1.Service{}, interactionRoute("OpenShiftRoute")} {
		name := w.Name
		if _, ok := object.(*corev1.Service); ok || object.GetObjectKind().GroupVersionKind() == openShiftRouteGVK {
			name = interactionName(w)
		}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, object); !apierrors.IsNotFound(err) {
			t.Fatalf("finalization retained %T: %v", object, err)
		}
	}
}

func TestInteractionRejectedWrongParentAndSuspended(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
		t.Fatal(err)
	}
	acceptHTTPRoute(t, r, w, 4)
	u := readInteractionRoute(t, r, w, "HTTPRoute")
	parents, _, _ := unstructured.NestedSlice(u.Object, "status", "parents")
	parents[0].(map[string]interface{})["parentRef"].(map[string]interface{})["namespace"] = "wrong-namespace"
	if err := unstructured.SetNestedSlice(u.Object, parents, "status", "parents"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Status == metav1.ConditionTrue {
		t.Fatal("accepted a different parent Gateway", err)
	}
	acceptHTTPRoute(t, r, w, 4)
	u = readInteractionRoute(t, r, w, "HTTPRoute")
	parents, _, _ = unstructured.NestedSlice(u.Object, "status", "parents")
	rejected := parents[0].(map[string]interface{})["conditions"].([]interface{})[1].(map[string]interface{})
	rejected["status"] = "False"
	rejected["reason"] = "BackendNotFound"
	rejected["message"] = "service missing"
	if err := unstructured.SetNestedSlice(u.Object, parents, "status", "parents"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Reason != "RouteRejected" {
		t.Fatalf("rejected reference: %+v %v", state, err)
	}
	acceptHTTPRoute(t, r, w, 4)
	w.Spec.OperatingMode = sandbox.Suspended
	state, err = r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Reason != "Suspended" || state.status.URL == "" {
		t.Fatalf("suspended endpoint: %+v %v", state, err)
	}
}

func TestInteractionDoesNotMutateForeignRouteOrRecreateStableResources(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	u := interactionRoute("HTTPRoute")
	u.SetName(interactionName(w))
	u.SetNamespace(w.Namespace)
	if err := r.Client.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err == nil {
		t.Fatal("adopted a foreign route")
	}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil || len(u.GetOwnerReferences()) != 0 {
		t.Fatal("changed foreign route", err)
	}
	if err := r.Client.Delete(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
		t.Fatal(err)
	}
	writes := 0
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
		writes++
		return c.Update(ctx, o, opts...)
	}, Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		writes++
		return c.Create(ctx, o, opts...)
	}})
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("stable resources rewritten %d times", writes)
	}
}

func TestInteractionNamesAreValidServiceNames(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range []string{"ordinary", "9agent", "agent.example", "agent-example", strings.Repeat("a", 253)} {
		w := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: name}}
		access := interactionName(w)
		if problems := validation.IsDNS1035Label(access); len(problems) != 0 {
			t.Fatalf("%q -> %q: %v", name, access, problems)
		}
		if seen[access] {
			t.Fatalf("normalized names collide: %q", access)
		}
		seen[access] = true
	}
}

func TestOpenShiftAdmissionCanUseAnyMatchingRouter(t *testing.T) {
	r, w, core := interactionFixture(t, "OpenShiftRoute")
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
		t.Fatal(err)
	}
	u := readInteractionRoute(t, r, w, "OpenShiftRoute")
	ingress := []interface{}{map[string]interface{}{"host": "agent.example.com", "conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "False", "reason": "HostAlreadyClaimed"}}}, map[string]interface{}{"host": "agent.example.com", "conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "True"}}}}
	if err := unstructured.SetNestedSlice(u.Object, ingress, "status", "ingress"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	state, err := r.reconcileInteraction(ctx, w, core, 7)
	if err != nil || state.condition.Status != metav1.ConditionTrue {
		t.Fatalf("one router accepted: %+v %v", state, err)
	}
}

func TestInteractionDisableCleansPartialCreationWithoutStatus(t *testing.T) {
	r, w, core := interactionFixture(t, "HTTPRoute")
	if _, err := r.reconcileInteraction(ctx, w, core, 7); err != nil {
		t.Fatal(err)
	}
	// Simulate a failed status write, followed by the user removing interaction.
	w.Spec.Interaction = nil
	w.Status = v1.AgentInstanceStatus{}
	for range 3 {
		if _, err := r.reconcileInteraction(ctx, w, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	key := client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}
	for _, o := range []client.Object{&corev1.Service{}, interactionRoute("HTTPRoute")} {
		if err := r.Client.Get(ctx, key, o); !apierrors.IsNotFound(err) {
			t.Fatalf("disabled endpoint retained %T: %v", o, err)
		}
	}
}

func TestRouteCleanupDoesNotBlockExecutionShutdown(t *testing.T) {
	r, w, _ := interactionFixture(t, "HTTPRoute")
	route := interactionRoute("HTTPRoute")
	route.SetName(interactionName(w))
	route.SetNamespace(w.Namespace)
	route.SetUID("route-uid")
	route.SetFinalizers([]string{"audit.example/route-cleanup"})
	if err := controllerutil.SetControllerReference(w, route, r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, route); err != nil {
		t.Fatal(err)
	}
	claim := r.allocationClaim(w)
	claim.SetUID("native-claim-uid")
	claim.SetFinalizers([]string{"audit.example/execution-cleanup"})
	if err := controllerutil.SetControllerReference(w, claim, r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		done, err := r.cleanupExecution(ctx, w)
		if err != nil || done {
			t.Fatalf("expected route cleanup to remain pending: %v %v", done, err)
		}
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	if claim.GetDeletionTimestamp() == nil {
		t.Fatal("route finalizer blocked the execution deletion request")
	}
}
