package agentinstance

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var httpRouteGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}

var openShiftRouteGVK = schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}

type interactionState struct {
	status    *v1.InteractionEndpointStatus
	condition *metav1.Condition
}

func (s *interactionState) pending(w *v1.AgentInstance, reason, message string) {
	s.condition = &metav1.Condition{Type: controller.ConditionInteractionReady, Status: metav1.ConditionFalse, ObservedGeneration: w.Generation, Reason: reason, Message: message}
}

func interactionName(w *v1.AgentInstance) string {
	const suffix = "-interaction"
	if len(validation.IsDNS1035Label(w.Name+suffix)) == 0 {
		return w.Name + suffix
	}
	hash := sha256.Sum256([]byte(w.Name))
	base := strings.ReplaceAll(w.Name, ".", "-")
	const prefix = "agentrally-"
	hashSuffix := fmt.Sprintf("-%x", hash[:8])
	maxBaseLength := 63 - len(prefix) - len(hashSuffix) - len(suffix)
	if len(base) > maxBaseLength {
		base = base[:maxBaseLength]
	}
	return prefix + strings.Trim(base, "-") + hashSuffix + suffix
}

func interactionRoute(kind string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	if kind == "HTTPRoute" {
		u.SetGroupVersionKind(httpRouteGVK)
	} else {
		u.SetGroupVersionKind(openShiftRouteGVK)
	}
	return u
}

func validateInteraction(e *v1.InteractionEndpoint) error {
	if e.Type != "HTTPRoute" && e.Type != "OpenShiftRoute" {
		return fmt.Errorf("interaction.type must be HTTPRoute or OpenShiftRoute")
	}
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("interaction.port must be between 1 and 65535")
	}
	if e.Scheme != "" && e.Scheme != "HTTP" && e.Scheme != "HTTPS" {
		return fmt.Errorf("interaction.scheme must be HTTP or HTTPS")
	}
	if e.Hostname != "" && len(validation.IsDNS1123Subdomain(e.Hostname)) != 0 {
		return fmt.Errorf("interaction.hostname must be a DNS hostname")
	}
	if e.Type == "HTTPRoute" {
		if e.Hostname == "" || e.GatewayRef == nil || len(validation.IsDNS1123Subdomain(e.GatewayRef.Name)) != 0 {
			return fmt.Errorf("HTTPRoute requires a hostname and valid gatewayRef.name")
		}
		if e.GatewayRef.Namespace != "" && len(validation.IsDNS1123Label(e.GatewayRef.Namespace)) != 0 {
			return fmt.Errorf("invalid gatewayRef.namespace")
		}
		if e.GatewayRef.SectionName != "" && len(validation.IsDNS1123Label(e.GatewayRef.SectionName)) != 0 {
			return fmt.Errorf("invalid gatewayRef.sectionName")
		}
	} else if e.GatewayRef != nil {
		return fmt.Errorf("gatewayRef is only valid for HTTPRoute")
	}
	return nil
}

func interactionAPIUnavailable(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}

// Optional route APIs use a direct client and periodic reconciliation. They need
// no informers or installed CRDs when the manager starts on a plain cluster.
func (r *Reconciler) cleanupInteraction(ctx context.Context, w *v1.AgentInstance, keep string) (bool, error) {
	done := true
	for _, kind := range []string{"HTTPRoute", "OpenShiftRoute"} {
		if kind == keep {
			continue
		}
		u := interactionRoute(kind)
		u.SetName(interactionName(w))
		u.SetNamespace(w.Namespace)
		c := r.directClient()
		if err := c.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
			if apierrors.IsNotFound(err) || interactionAPIUnavailable(err) {
				continue
			}
			return false, err
		}
		// An unrelated object with this name is never a cleanup target.
		if controller.CheckOwner(u, w) != nil {
			continue
		}
		deleted, err := controller.DeleteOwned(ctx, c, u, w)
		if err != nil {
			return false, err
		}
		done = done && deleted
	}
	if !done || keep != "" {
		return done, nil
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: interactionName(w), Namespace: w.Namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}
	if controller.CheckOwner(service, w) != nil {
		return true, nil
	}
	return controller.DeleteOwned(ctx, r.Client, service, w)
}

func hasInteraction(w *v1.AgentInstance) bool {
	return w.Spec.Interaction != nil || w.Status.Interaction != nil || meta.FindStatusCondition(w.Status.Conditions, controller.ConditionInteractionReady) != nil
}

func (r *Reconciler) reconcileInteraction(ctx context.Context, w *v1.AgentInstance, core *sandbox.Sandbox, generation int64) (interactionState, error) {
	state := interactionState{}
	if w.Spec.Interaction != nil {
		group := &v1.AgentGroup{}
		if err := r.referenceReader().Get(ctx, client.ObjectKey{Name: w.Spec.AgentGroupRef.Name}, group); err != nil {
			return state, err
		}
		if controller.ManagedInvocation(group) {
			_, err := r.cleanupInteraction(ctx, w, "")
			state.pending(w, "InvocationAccessConflict", "ConnectorOnly does not allow external interaction routes")
			return state, err
		}
	}
	if !hasInteraction(w) {
		// A previous attempt may have created access resources before its status
		// write failed. Disabling must not depend only on recorded status history.
		service := &corev1.Service{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: interactionName(w)}, service); err != nil {
			return state, client.IgnoreNotFound(err)
		}
		if controller.CheckOwner(service, w) != nil {
			return state, nil
		}
	}
	if w.Spec.Interaction == nil {
		done, err := r.cleanupInteraction(ctx, w, "")
		if err != nil {
			return state, err
		}
		state.pending(w, "Disabled", "Interaction endpoint is disabled")
		if !done {
			state.pending(w, "Deleting", "Waiting for interaction resources to finish deletion")
		}
		return state, nil
	}
	e, err := r.resolveInteraction(ctx, w, core)
	if err != nil {
		if interactionAPIUnavailable(err) {
			state.pending(w, "APIUnavailable", "The selected interaction route API is not installed")
			return state, nil
		}
		reason, message := diagnostics.Classify(err)
		state.pending(w, reason, message)
		if diagnostics.IsPending(err) {
			return state, nil
		}
		return state, err
	}
	resolved := w.DeepCopy()
	resolved.Spec.Interaction = e
	return r.reconcileResolvedInteraction(ctx, resolved, core, generation)
}

func (r *Reconciler) reconcileResolvedInteraction(ctx context.Context, w *v1.AgentInstance, core *sandbox.Sandbox, generation int64) (interactionState, error) {
	state := interactionState{}
	e := w.Spec.Interaction
	done, err := r.cleanupInteraction(ctx, w, e.Type)
	if err != nil {
		return state, err
	}
	if !done {
		state.pending(w, "Deleting", "Waiting for the previous route type to finish deletion")
		return state, nil
	}
	state.pending(w, "SandboxPending", "Waiting for Core to publish a Pod selector")
	if core == nil || core.Status.LabelSelector == "" {
		return state, nil
	}
	service, err := r.ensureInteractionService(ctx, w, core)
	if err != nil {
		return state, err
	}
	route, err := r.ensureInteractionRoute(ctx, w, service)
	if interactionAPIUnavailable(err) {
		state.pending(w, "APIUnavailable", "The selected interaction route API is not installed")
		return state, nil
	}
	if err != nil {
		return state, err
	}
	return observeInteraction(w, core, generation, route), nil
}

func (r *Reconciler) ensureInteractionService(ctx context.Context, w *v1.AgentInstance, core *sandbox.Sandbox) (*corev1.Service, error) {
	e := w.Spec.Interaction
	selector, err := metav1.ParseToLabelSelector(core.Status.LabelSelector)
	if err != nil {
		return nil, fmt.Errorf("core selector: %w", err)
	}
	match, err := metav1.LabelSelectorAsMap(selector)
	if err != nil || len(match) == 0 {
		return nil, fmt.Errorf("core selector must contain nonempty equality matches")
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: interactionName(w), Namespace: w.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if err := controller.PrepareOwned(service, w, r.Scheme); err != nil {
			return err
		}
		// Preserve allocated ClusterIP/ClusterIPs and other admission defaults.
		service.Spec.Type = corev1.ServiceTypeClusterIP
		service.Spec.Selector = match
		service.Spec.Ports = []corev1.ServicePort{{
			Name: "http", Protocol: corev1.ProtocolTCP,
			Port: 80, TargetPort: intstr.FromInt32(e.Port),
		}}
		return nil
	})
	return service, err
}

// Preserve the route controller's assigned hostname when the user omits it.
func (r *Reconciler) ensureInteractionRoute(ctx context.Context, w *v1.AgentInstance, service *corev1.Service) (*unstructured.Unstructured, error) {
	e := w.Spec.Interaction
	route := interactionRoute(e.Type)
	route.SetName(service.Name)
	route.SetNamespace(w.Namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.directClient(), route, func() error {
		if route.GetDeletionTimestamp() != nil {
			return fmt.Errorf("interaction route is terminating")
		}
		if route.GetResourceVersion() != "" {
			if err := controller.CheckOwner(route, w); err != nil {
				return err
			}
		}
		// Preserve the controller-assigned hostname when the user omits it.
		assigned, _, _ := unstructured.NestedString(route.Object, "spec", "host")
		spec := interactionRouteSpec(w, assigned)
		if err := unstructured.SetNestedMap(route.Object, spec, "spec"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(w, route, r.Scheme)
	})
	return route, err
}

func observeInteraction(w *v1.AgentInstance, core *sandbox.Sandbox, generation int64, route *unstructured.Unstructured) interactionState {
	e := w.Spec.Interaction
	state := interactionState{}
	state.status = &v1.InteractionEndpointStatus{ServiceName: interactionName(w), RouteName: route.GetName(), Port: e.Port, GatewayRef: e.GatewayRef.DeepCopy()}
	hostname := e.Hostname
	if e.Type == "OpenShiftRoute" {
		hostname, _, _ = unstructured.NestedString(route.Object, "spec", "host")
	}
	scheme := e.Scheme
	if scheme == "" {
		scheme = "HTTPS"
	}
	if hostname != "" {
		state.status.URL = strings.ToLower(scheme) + "://" + hostname
	}
	state.pending(w, "RoutePending", "Waiting for the route controller to accept the endpoint")
	accepted, reason, message := interactionAccepted(w, route, hostname)
	if !accepted {
		if reason != "" {
			state.pending(w, reason, message)
		}
		return state
	}
	if executionMode(w) == sandbox.Suspended {
		state.pending(w, "Suspended", "Route is retained while the Agent is suspended")
		return state
	}
	ready := meta.FindStatusCondition(core.Status.Conditions, controller.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != generation {
		state.pending(w, "SandboxPending", "Route is accepted; waiting for current Core readiness")
		return state
	}
	state.condition = &metav1.Condition{
		Type: controller.ConditionInteractionReady, Status: metav1.ConditionTrue, ObservedGeneration: w.Generation,
		Reason:  "RouteAccepted",
		Message: "Route controller accepted the endpoint and Core reports Ready; connectivity is not probed",
	}
	return state
}

func interactionRouteSpec(w *v1.AgentInstance, assignedHost string) map[string]interface{} {
	e := w.Spec.Interaction
	name := interactionName(w)
	if e.Type == "HTTPRoute" {
		namespace := e.GatewayRef.Namespace
		if namespace == "" {
			namespace = w.Namespace
		}
		parent := map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": e.GatewayRef.Name, "namespace": namespace}
		if e.GatewayRef.SectionName != "" {
			parent["sectionName"] = e.GatewayRef.SectionName
		}
		rule := map[string]interface{}{
			"matches": []interface{}{map[string]interface{}{"path": map[string]interface{}{"type": "PathPrefix", "value": "/"}}},
			"backendRefs": []interface{}{map[string]interface{}{
				"group": "", "kind": "Service", "name": name, "port": int64(80), "weight": int64(1),
			}},
		}
		return map[string]interface{}{
			"parentRefs": []interface{}{parent}, "hostnames": []interface{}{e.Hostname}, "rules": []interface{}{rule},
		}
	}
	spec := map[string]interface{}{
		"to":   map[string]interface{}{"kind": "Service", "name": name, "weight": int64(100)},
		"port": map[string]interface{}{"targetPort": "http"}, "wildcardPolicy": "None",
	}
	host := e.Hostname
	if host == "" {
		host = assignedHost
	}
	if host != "" {
		spec["host"] = host
	}
	if e.Scheme != "HTTP" {
		spec["tls"] = map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"}
	}
	return spec
}

func interactionAccepted(w *v1.AgentInstance, route *unstructured.Unstructured, hostname string) (bool, string, string) {
	if w.Spec.Interaction.Type == "OpenShiftRoute" {
		return openShiftRouteAccepted(route, hostname)
	}
	return httpRouteAccepted(w, route)
}

func openShiftRouteAccepted(route *unstructured.Unstructured, hostname string) (bool, string, string) {
	rejected := ""
	ingress, _, _ := unstructured.NestedSlice(route.Object, "status", "ingress")
	for _, item := range ingress {
		entry, ok := item.(map[string]interface{})
		if !ok || entry["host"] != hostname || hostname == "" {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(entry, "conditions")
		for _, item := range conditions {
			c, ok := item.(map[string]interface{})
			if !ok || c["type"] != "Admitted" {
				continue
			}
			if c["status"] == "True" {
				return true, "", ""
			}
			if c["status"] == "False" {
				rejected = fmt.Sprint(c["reason"], ": ", c["message"])
			}
		}
	}
	if rejected != "" {
		return false, "RouteRejected", rejected
	}
	return false, "", ""
}

func httpRouteAccepted(w *v1.AgentInstance, route *unstructured.Unstructured) (bool, string, string) {
	rejected := ""
	ref := w.Spec.Interaction.GatewayRef
	namespace := ref.Namespace
	if namespace == "" {
		namespace = w.Namespace
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	for _, item := range parents {
		p, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		parent, _, _ := unstructured.NestedMap(p, "parentRef")
		if !matchesGatewayParent(parent, ref, w.Namespace, namespace) {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(p, "conditions")
		accepted, resolved := false, false
		parentRejected := false
		for _, item := range conditions {
			c, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			generation, _, _ := unstructured.NestedInt64(c, "observedGeneration")
			if generation != route.GetGeneration() {
				continue
			}
			if c["type"] != "Accepted" && c["type"] != "ResolvedRefs" {
				continue
			}
			if c["status"] == "False" {
				rejected = fmt.Sprint(c["reason"], ": ", c["message"])
				parentRejected = true
			}
			if c["status"] == "True" {
				if c["type"] == "Accepted" {
					accepted = true
				}
				if c["type"] == "ResolvedRefs" {
					resolved = true
				}
			}
		}
		if accepted && resolved && !parentRejected {
			return true, "", ""
		}
	}
	if rejected != "" {
		return false, "RouteRejected", rejected
	}
	return false, "", ""
}

func matchesGatewayParent(parent map[string]interface{}, ref *v1.GatewayReference, routeNamespace, gatewayNamespace string) bool {
	parentNamespace, _ := parent["namespace"].(string)
	if parentNamespace == "" {
		parentNamespace = routeNamespace
	}
	if parent["name"] != ref.Name || parentNamespace != gatewayNamespace || (ref.SectionName != "" && parent["sectionName"] != ref.SectionName) {
		return false
	}
	if group, ok := parent["group"]; ok && group != "gateway.networking.k8s.io" {
		return false
	}
	if kind, ok := parent["kind"]; ok && kind != "Gateway" {
		return false
	}
	return true
}
