package connector

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/podconfig"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type invocationTarget struct {
	Address     string
	Host        string
	ExecutionID string
}

// resolveInvocation performs direct reads; it does not mutate or wake instances.
func (e Executor) resolveInvocation(ctx context.Context, identity InvocationIdentity, peer controller.InvocationPeer) (invocationTarget, error) {
	if err := validateInvocationIdentity(identity); err != nil {
		return invocationTarget{}, err
	}
	groupObject, namespace, err := e.scopedGroup(ctx, &identity.Scope)
	if err != nil {
		return invocationTarget{}, err
	}
	var group v1.AgentGroup
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(groupObject.Object, &group); err != nil {
		return invocationTarget{}, err
	}
	if group.DeletionTimestamp != nil || !controller.ManagedInvocation(&group) {
		return invocationTarget{}, invocationError("InvocationAccessDisabled")
	}
	req := Request{Version: "v2", Method: http.MethodGet, Resource: "instanceobservations", Name: identity.InstanceID, Namespace: namespace, Scope: &identity.Scope}
	observed, err := e.observeExecution(ctx, req)
	if err != nil {
		return invocationTarget{}, err
	}
	if string(observed.Instance.UID) != identity.InstanceUID || observed.Observation.Execution == nil || observed.Observation.Execution.ID != identity.ExecutionID {
		return invocationTarget{}, invocationError("ExecutionChanged")
	}
	if observed.Observation.State != "Active" || observed.Pod == nil || observed.Sandbox == nil {
		return invocationTarget{}, invocationError("ExecutionNotActive")
	}
	if observed.Pod.Spec.HostNetwork {
		return invocationTarget{}, invocationError("HostNetworkUnsupported")
	}
	if err := e.verifyInvocationPolicy(ctx, &group, peer); err != nil {
		return invocationTarget{}, err
	}
	port, err := e.invocationPort(ctx, observed)
	if err != nil {
		return invocationTarget{}, err
	}
	ip := observed.Pod.Status.PodIP
	if !validPodIP(ip) {
		return invocationTarget{}, invocationError("InvalidPodAddress")
	}
	serviceEnabled, found, err := unstructured.NestedBool(observed.Sandbox.Object, "spec", "service")
	if err != nil || !found {
		return invocationTarget{}, invocationError("InvalidServiceConfiguration")
	}
	host := net.JoinHostPort(ip, strconv.Itoa(int(port)))
	if serviceEnabled {
		if err := e.verifyInvocationService(ctx, observed, port); err != nil {
			return invocationTarget{}, err
		}
		host = net.JoinHostPort(observed.Sandbox.GetName()+"."+namespace+".svc", strconv.Itoa(int(port)))
	}
	// This check catches changes during resolution. It is not process fencing.
	current, err := e.observeExecution(ctx, req)
	if err != nil {
		return invocationTarget{}, err
	}
	if current.Observation.Execution == nil || current.Observation.Execution.ID != identity.ExecutionID || current.Observation.State != "Active" {
		return invocationTarget{}, invocationError("ExecutionChanged")
	}
	return invocationTarget{Address: net.JoinHostPort(ip, strconv.Itoa(int(port))), Host: host, ExecutionID: identity.ExecutionID}, nil
}

func validPodIP(value string) bool {
	address, err := netip.ParseAddr(value)
	return err == nil && address.Zone() == "" && address.IsGlobalUnicast() && !address.IsLoopback() && !address.IsLinkLocalUnicast()
}

func (e Executor) invocationPort(ctx context.Context, observed *observedExecution) (int32, error) {
	binding := observed.Instance.Status.Definition
	if binding == nil || binding.InvocationProtocol != "HTTPJSONSSEV1" {
		return 0, invocationError("InvocationProtocolUndeclared")
	}
	object, err := e.Client.Resource(nativeResource("agentdefinitions")).Namespace(observed.Instance.Namespace).Get(ctx, binding.Name, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	if object.GetUID() != binding.UID {
		return 0, invocationError("DefinitionChanged")
	}
	if err := checkGroupReference(object, observed.Instance.Spec.AgentGroupRef.Name); err != nil {
		return 0, err
	}
	var declared v1.AgentDefinition
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &declared); err != nil {
		return 0, err
	}
	template, err := e.Client.Resource(sandbox.ExtensionsGroupVersion.WithResource("sandboxtemplates")).Namespace(declared.Namespace).Get(ctx, definition.Name(binding.UID, binding.Revision), metav1.GetOptions{})
	if err != nil {
		return 0, err
	}

	revision, err := definition.Verify(&declared, template, binding.Revision)
	if err != nil {
		return 0, err
	}
	if revision.Spec.InvocationProtocol != binding.InvocationProtocol {
		return 0, invocationError("DefinitionChanged")
	}
	pod, found, err := sandbox.TemplatePod(template)
	if err != nil || !found {
		return 0, invocationError("InvalidPublishedTemplate")
	}
	index := podconfig.AgentIndex(pod.Spec.Containers, revision.Spec.AgentContainer)
	if index < 0 || pod.Spec.Containers[index].Name != binding.AgentContainer {
		return 0, invocationError("AgentContainerUnavailable")
	}
	port, err := podconfig.InvocationPort(pod.Spec.Containers[index])
	if err != nil {
		return 0, invocationError("AmbiguousInvocationPort")
	}
	index = podconfig.AgentIndex(observed.Pod.Spec.Containers, binding.AgentContainer)
	if index < 0 {
		return 0, invocationError("AgentContainerUnavailable")
	}
	actual, err := podconfig.InvocationPort(observed.Pod.Spec.Containers[index])
	if err != nil || actual != port {
		return 0, invocationError("InvocationPortChanged")
	}
	return port, nil
}

func (e Executor) verifyInvocationPolicy(ctx context.Context, group *v1.AgentGroup, peer controller.InvocationPeer) error {
	expected, err := controller.GroupNetworkPolicy(group, peer)
	if err != nil {
		return invocationError("InvocationPolicyUnavailable")
	}
	policies, err := e.Client.Resource(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}).Namespace(group.NamespaceName()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	found := false
	for _, object := range policies.Items {
		var policy networkingv1.NetworkPolicy
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &policy); err != nil {
			return err
		}
		if policy.Name != controller.GroupNetworkPolicyName {
			if controller.IngressPolicy(&policy) {
				return invocationError("PolicyConflict")
			}
			continue
		}
		owner := metav1.GetControllerOf(&policy)
		if owner == nil || controller.CheckOwner(&policy, controller.GroupPolicyOwner(group)) != nil || policy.DeletionTimestamp != nil || !equality.Semantic.DeepEqual(policy.Spec, *expected) {
			return invocationError("InvocationPolicyUnavailable")
		}
		found = true
	}
	if !found {
		return invocationError("InvocationPolicyUnavailable")
	}
	instances, err := e.Client.Resource(nativeResource("agentinstances")).Namespace(group.NamespaceName()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, object := range instances.Items {
		for _, field := range []string{"spec", "status"} {
			if value, exists, _ := unstructured.NestedFieldNoCopy(object.Object, field, "interaction"); exists && value != nil {
				return invocationError("InteractionConflict")
			}
		}
	}
	return nil
}

func (e Executor) verifyInvocationService(ctx context.Context, observed *observedExecution, port int32) error {
	object, err := e.Client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "services"}).Namespace(observed.Instance.Namespace).Get(ctx, observed.Sandbox.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := requireOwner(object, observed.Sandbox, sandbox.GroupVersion.String(), "Sandbox"); err != nil {
		return err
	}
	var service corev1.Service
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &service); err != nil {
		return err
	}
	if service.Spec.ClusterIP != corev1.ClusterIPNone || service.DeletionTimestamp != nil || len(service.Spec.Selector) == 0 || !labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(observed.Pod.Labels)) {
		return invocationError("InvalidInvocationService")
	}
	slicesList, err := e.Client.Resource(schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}).Namespace(service.Namespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + service.Name})
	if err != nil {
		return err
	}
	found := false
	for _, item := range slicesList.Items {
		if err := requireOwner(&item, object, "v1", "Service"); err != nil {
			return err
		}
		var endpoints discoveryv1.EndpointSlice
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &endpoints); err != nil {
			return err
		}
		if endpoints.DeletionTimestamp != nil {
			return invocationError("EndpointChanged")
		}
		hasPort := slices.ContainsFunc(endpoints.Ports, func(p discoveryv1.EndpointPort) bool {
			return p.Port != nil && *p.Port == port && p.Protocol != nil && *p.Protocol == corev1.ProtocolTCP
		})
		if !hasPort {
			return invocationError("InvocationPortChanged")
		}
		for _, endpoint := range endpoints.Endpoints {
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
				continue
			}
			// Kubernetes EndpointSlice target refs omit apiVersion for core Pods.
			// Still require the exact Pod kind, namespace, name, and UID.
			ref := endpoint.TargetRef
			if ref == nil || (ref.APIVersion != "" && ref.APIVersion != "v1") || ref.Kind != "Pod" || ref.Namespace != service.Namespace || ref.Name != observed.Pod.Name || ref.UID != observed.Pod.UID {
				return invocationError("AmbiguousInvocationEndpoint")
			}
			for _, address := range endpoint.Addresses {
				if !validPodIP(address) || !slices.ContainsFunc(observed.Pod.Status.PodIPs, func(ip corev1.PodIP) bool { return ip.IP == address }) {
					return invocationError("EndpointAddressChanged")
				}
				if address == observed.Pod.Status.PodIP {
					found = true
				}
			}
		}
	}
	if !found {
		return invocationError("InvocationEndpointUnavailable")
	}
	return nil
}
