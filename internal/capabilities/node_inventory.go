package capabilities

import (
	"sort"
	"strings"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
)

// Reuse the runtime scan's non-terminating nodes without its scheduling filters.
func observeNodeInventory(nodes []runtimeNode, status *v1.ClusterCapabilitiesStatus) {
	type platform struct{ os, architecture string }
	platforms := map[platform]int32{}
	resources := map[corev1.ResourceName]*v1.NodeResourceObservation{}
	for _, entry := range nodes {
		node := entry.node
		platforms[platform{node.Status.NodeInfo.OperatingSystem, node.Status.NodeInfo.Architecture}]++
		names := map[corev1.ResourceName]bool{}
		for _, quantities := range []corev1.ResourceList{node.Status.Capacity, node.Status.Allocatable} {
			for name := range quantities {
				// Native Node resource names are API-validated. Like Kubernetes,
				// exclude unqualified, reserved-domain and quota-prefixed names.
				value := string(name)
				if strings.Contains(value, "/") && !strings.Contains(value, corev1.ResourceDefaultNamespacePrefix) && !strings.HasPrefix(value, corev1.DefaultResourceRequestsPrefix) {
					names[name] = true
				}
			}
		}
		for name := range names {
			observed := resources[name]
			if observed == nil {
				observed = &v1.NodeResourceObservation{Name: name}
				resources[name] = observed
			}
			observed.Nodes++
			observed.Capacity.Add(node.Status.Capacity[name])
			observed.Allocatable.Add(node.Status.Allocatable[name])
		}
	}
	for key, count := range platforms {
		status.Platforms = append(status.Platforms, v1.NodePlatformObservation{OS: key.os, Architecture: key.architecture, Nodes: count})
	}
	sort.Slice(status.Platforms, func(i, j int) bool {
		a, b := status.Platforms[i], status.Platforms[j]
		if a.OS != b.OS {
			return a.OS < b.OS
		}
		return a.Architecture < b.Architecture
	})
	for _, observed := range resources {
		status.ExtendedResources = append(status.ExtendedResources, *observed)
	}
	sort.Slice(status.ExtendedResources, func(i, j int) bool { return status.ExtendedResources[i].Name < status.ExtendedResources[j].Name })
}
