package capabilities

import (
	"context"
	"sort"

	v1 "agentrally/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NetworkProviderName identifies catalog candidates without making API reads or
// evaluating namespace trust and effective policy configuration.
func NetworkProviderName(daemon *appsv1.DaemonSet) string {
	if daemon == nil {
		return ""
	}
	rule, _ := networkRule(daemon)
	return rule.Provider
}

func networkProvider(ctx context.Context, reader client.Reader, ds appsv1.DaemonSet) (v1.NetworkPolicyProviderObservation, bool, error) {
	rule, recognized := networkRule(&ds)
	p := v1.NetworkPolicyProviderObservation{Name: ds.Name, Namespace: ds.Namespace, Provider: rule.Provider, Status: "Unknown", Source: "BuiltInCatalog", Rule: "network/" + rule.Provider}
	if !recognized {
		return p, false, nil
	}
	trusted, err := trustedNetworkNamespace(ctx, reader, ds.Namespace)
	if err != nil || !trusted {
		return p, false, err
	}
	var containers []corev1.Container
	for _, c := range ds.Spec.Template.Spec.Containers {
		if len(rule.Containers) == 0 || oneOf(c.Name, rule.Containers...) {
			containers = append(containers, c)
		}
	}
	if len(rule.Containers) > 0 && len(containers) == 0 {
		if rule.MissingContainerStatus == "" {
			return p, false, nil
		}
		p.Status, p.Reason = rule.MissingContainerStatus, "NetworkPolicyControllerNotObserved"
	} else {
		p.Status, p.Reason, err = networkConfiguration(ctx, reader, &ds, rule, containers)
		if err != nil {
			return p, true, err
		}
	}
	if ds.DeletionTimestamp != nil || ds.Status.ObservedGeneration != ds.Generation || ds.Status.DesiredNumberScheduled < 1 || ds.Status.NumberReady < ds.Status.DesiredNumberScheduled {
		p.Status, p.Reason = "Unknown", "NetworkPolicyProviderNotReady"
	}
	return p, true, nil
}

// System installation namespaces and explicitly approved custom namespaces are
// administrator-controlled. Names in ordinary tenant namespaces are not evidence.
func trustedNetworkNamespace(ctx context.Context, reader client.Reader, name string) (bool, error) {
	if oneOf(name, "kube-system", "calico-system", "kube-flannel") {
		return true, nil
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: name}, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return namespace.DeletionTimestamp == nil && namespace.Labels["arally.io/network-discovery"] == "trusted", nil
}

func discoverNetwork(ctx context.Context, reader client.Reader, status *v1.ClusterCapabilitiesStatus) error {
	network := &v1.NetworkPolicyObservation{Status: "Unknown", Reason: "NetworkPolicyProviderNotRecognized"}
	status.NetworkPolicy = network
	daemons := &appsv1.DaemonSetList{}
	if err := reader.List(ctx, daemons); err != nil {
		network.Reason = "DiscoveryFailed"
		return err
	}
	sort.Slice(daemons.Items, func(i, j int) bool {
		a, b := daemons.Items[i], daemons.Items[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	var providers []v1.NetworkPolicyProviderObservation
	for _, ds := range daemons.Items {
		p, recognized, err := networkProvider(ctx, reader, ds)
		if err != nil {
			network.Reason = "DiscoveryFailed"
			return err
		}
		if recognized {
			providers = append(providers, p)
		}
	}
	network.Providers = providers
	for _, p := range providers {
		if p.Status == "Supported" {
			network.Status = "Supported"
			network.Reason = "ReadyProviderConfigurationObserved"
			return nil
		}
	}
	// Provider-scoped negatives cannot exclude an unrecognized coexisting engine.
	// Passive discovery never proves global absence of NetworkPolicy enforcement.
	if len(providers) > 0 {
		network.Reason = "NoConfiguredPolicyEngineObserved"
	}
	return nil
}
