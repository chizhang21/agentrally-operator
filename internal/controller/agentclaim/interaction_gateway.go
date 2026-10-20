package agentclaim

import (
	"context"
	"fmt"
	"strings"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var gatewayGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"}

type interactionListener struct {
	ref              *v1.GatewayReference
	scheme, hostname string
	isDefault        bool
}

func (r *Reconciler) selectInteractionGateway(ctx context.Context, w *v1.AgentClaim, e *v1.InteractionEndpoint) (*interactionListener, error) {
	gateways, err := r.interactionGateways(ctx, w, e.GatewayRef)
	if err != nil {
		return nil, err
	}
	var candidates []interactionListener
	for i := range gateways {
		matches, err := r.interactionListeners(ctx, w, e, &gateways[i])
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, matches...)
	}
	return selectInteractionListener(candidates, e)
}

func selectInteractionListener(candidates []interactionListener, e *v1.InteractionEndpoint) (*interactionListener, error) {
	if len(candidates) == 0 {
		return nil, interactionDiagnostic("GatewayUnavailable", "No ready Gateway listener supports this HTTPRoute, namespace, hostname and scheme")
	}
	if e.GatewayRef == nil {
		var defaults []interactionListener
		for _, c := range candidates {
			if c.isDefault {
				defaults = append(defaults, c)
			}
		}
		if len(defaults) > 0 {
			candidates = defaults
		}
	}
	gatewaysSeen := map[string]bool{}
	for _, c := range candidates {
		gatewaysSeen[c.ref.Namespace+"/"+c.ref.Name] = true
	}
	if len(gatewaysSeen) > 1 {
		return nil, interactionDiagnostic("GatewayAmbiguous", "Multiple eligible Gateways exist; select one in Group interactionDefaults.httpRoute.gatewayRef or Claim interaction.gatewayRef")
	}
	if e.Scheme == "" {
		var secure []interactionListener
		for _, c := range candidates {
			if c.scheme == "HTTPS" {
				secure = append(secure, c)
			}
		}
		if len(secure) > 0 {
			candidates = secure
		}
	}
	if len(candidates) != 1 {
		return nil, interactionDiagnostic("GatewayListenerAmbiguous", "Multiple eligible listeners exist; select gatewayRef.sectionName")
	}
	return &candidates[0], nil
}

func (r *Reconciler) interactionGateways(ctx context.Context, w *v1.AgentClaim, ref *v1.GatewayReference) ([]unstructured.Unstructured, error) {
	if ref != nil {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gatewayGVK)
		ns := ref.Namespace
		if ns == "" {
			ns = w.Namespace
		}
		err := r.interactionClient().Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj)
		if apierrors.IsNotFound(err) {
			return nil, interactionDiagnostic("GatewayNotFound", "The selected Gateway does not exist")
		}
		if err != nil {
			return nil, err
		}
		return []unstructured.Unstructured{*obj}, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gatewayGVK.GroupVersion().WithKind("GatewayList"))
	if err := r.interactionClient().List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *Reconciler) interactionListeners(ctx context.Context, w *v1.AgentClaim, e *v1.InteractionEndpoint, g *unstructured.Unstructured) ([]interactionListener, error) {
	if g.GetDeletionTimestamp() != nil || !currentGatewayCondition(g.Object, g.GetGeneration(), "Accepted") || !currentGatewayCondition(g.Object, g.GetGeneration(), "Programmed") {
		return nil, nil
	}
	listeners, _, _ := unstructured.NestedSlice(g.Object, "spec", "listeners")
	defaults, _, _ := unstructured.NestedString(g.Object, "spec", "defaultScope")
	var matches []interactionListener
	for _, value := range listeners {
		l, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := l["name"].(string)
		protocol, _ := l["protocol"].(string)
		hostname, _ := l["hostname"].(string)
		if (protocol != "HTTP" && protocol != "HTTPS") || (e.Scheme != "" && e.Scheme != protocol) || (e.GatewayRef != nil && e.GatewayRef.SectionName != "" && e.GatewayRef.SectionName != name) {
			continue
		}
		if !gatewayHostnameMatches(hostname, e.Hostname) || !gatewayListenerReady(g, name) {
			continue
		}
		allowed, err := r.gatewayAllowsNamespace(ctx, w.Namespace, g.GetNamespace(), l)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		matches = append(matches, interactionListener{
			ref:    &v1.GatewayReference{Name: g.GetName(), Namespace: g.GetNamespace(), SectionName: name},
			scheme: protocol, hostname: hostname,
			isDefault: defaults == "All" && currentGatewayCondition(g.Object, g.GetGeneration(), "DefaultGateway"),
		})
	}
	return matches, nil
}

func currentGatewayCondition(object map[string]interface{}, generation int64, kind string) bool {
	conditions, _, _ := unstructured.NestedSlice(object, "status", "conditions")
	for _, value := range conditions {
		c, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		observed, _, _ := unstructured.NestedInt64(c, "observedGeneration")
		if c["type"] == kind {
			return c["status"] == "True" && observed == generation
		}
	}
	return false
}

func gatewayListenerReady(g *unstructured.Unstructured, name string) bool {
	listeners, _, _ := unstructured.NestedSlice(g.Object, "status", "listeners")
	for _, value := range listeners {
		l, ok := value.(map[string]interface{})
		if !ok || l["name"] != name {
			continue
		}
		kinds, _, _ := unstructured.NestedSlice(l, "supportedKinds")
		supported := false
		for _, value := range kinds {
			if k, ok := value.(map[string]interface{}); ok && k["kind"] == "HTTPRoute" && (k["group"] == nil || k["group"] == "gateway.networking.k8s.io") {
				supported = true
			}
		}
		object := map[string]interface{}{"status": map[string]interface{}{"conditions": l["conditions"]}}
		return supported && currentGatewayCondition(object, g.GetGeneration(), "Accepted") && currentGatewayCondition(object, g.GetGeneration(), "ResolvedRefs")
	}
	return false
}

func (r *Reconciler) gatewayAllowsNamespace(ctx context.Context, namespace, gatewayNamespace string, listener map[string]interface{}) (bool, error) {
	kinds, _, _ := unstructured.NestedSlice(listener, "allowedRoutes", "kinds")
	if len(kinds) > 0 {
		supported := false
		for _, value := range kinds {
			if k, ok := value.(map[string]interface{}); ok && k["kind"] == "HTTPRoute" && (k["group"] == nil || k["group"] == "gateway.networking.k8s.io") {
				supported = true
			}
		}
		if !supported {
			return false, nil
		}
	}
	from, _, _ := unstructured.NestedString(listener, "allowedRoutes", "namespaces", "from")
	switch from {
	case "All":
		return true, nil
	case "", "Same":
		return namespace == gatewayNamespace, nil
	case "Selector":
		raw, found, err := unstructured.NestedMap(listener, "allowedRoutes", "namespaces", "selector")
		if err != nil || !found {
			return false, nil
		}
		selector := &metav1.LabelSelector{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, selector); err != nil {
			return false, fmt.Errorf("gateway namespace selector: %w", err)
		}
		selected, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return false, err
		}
		ns := &corev1.Namespace{}
		if err := r.referenceReader().Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
			return false, diagnostics.ReferenceError("Namespace", "", namespace, err)
		}
		return selected.Matches(labels.Set(ns.Labels)), nil
	default:
		return false, nil
	}
}

func gatewayHostnameMatches(listener, route string) bool {
	if listener == "" || route == "" {
		return true
	}
	if strings.HasPrefix(listener, "*.") {
		return strings.HasSuffix(route, listener[1:]) && len(route) > len(listener)-1
	}
	return listener == route
}
