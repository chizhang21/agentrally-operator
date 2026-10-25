package capabilities

import (
	"sort"

	v1 "arally/operator/api/v1alpha1"
)

// summarize groups audited inventory without reading APIs or renewing evidence.
// Resource-level uncertainty remains visible even when another choice in the
// same category has positive configuration evidence.
func summarize(status v1.ClusterCapabilitiesStatus) []v1.CapabilityObservation {
	groups := map[string]*v1.CapabilityObservation{}
	add := func(name, title, category, description string, resource *v1.CapabilityReference) {
		group := groups[name]
		if group == nil {
			group = &v1.CapabilityObservation{Name: name, DisplayName: title, Category: category, Description: description, Status: "Unknown"}
			groups[name] = group
		}
		if resource != nil {
			group.Resources = append(group.Resources, *resource)
			if resource.Status == "Observed" {
				group.Status = "Observed"
			}
		}
	}
	if status.DefaultRuntime != nil {
		add("DefaultRuntime", "Node default runtime", "Isolation", "The node default executor cannot be identified from CRI implementation names.", nil)
	}
	for _, runtime := range status.Runtimes {
		name, title, description := "UnclassifiedRuntime", "Unclassified runtime", "The configured executor's isolation model is unknown."
		switch runtime.Category {
		case "Container":
			name, title, description = "ContainerIsolation", "Standard containers", "Containers share the host kernel; execution has not been probed."
		case "UserSpaceKernel":
			name, title, description = "UserSpaceKernelIsolation", "Application kernel isolation", "A userspace application kernel isolates workloads; execution has not been probed."
		case "VirtualMachine":
			name, title, description = "VirtualMachineIsolation", "Virtual machine isolation", "Workloads use a separate guest kernel; VM startup and confidential computing are not verified."
		case "WebAssembly":
			name, title, description = "WebAssemblyExecution", "WebAssembly execution", "A Wasm/WASI workload contract is required; ordinary Linux application images are not assumed compatible."
		}
		add(name, title, "Isolation", description, &v1.CapabilityReference{Kind: "RuntimeClass", Name: runtime.Name, Status: runtime.Status, Source: runtime.Source, Reason: runtime.Reason})
		if runtime.Features == nil {
			continue
		}
		for _, feature := range []struct {
			name, title, category, description string
			reports                            v1.RuntimeFeatureObservation
		}{
			{"UserNamespaces", "Pod user namespaces", "Isolation", "Container users can map to different host users when Pods opt in; node reports do not verify workload execution.", runtime.Features.UserNamespaces},
			{"RecursiveReadOnlyMounts", "Recursive read-only mounts", "Storage", "Recursive read-only volume mounts require workload opt-in; node reports do not verify mounted volumes.", runtime.Features.RecursiveReadOnlyMounts},
		} {
			resource := v1.CapabilityReference{Kind: "RuntimeClass", Name: runtime.Name, Status: "Unknown", Source: "NativeAPI", Reason: "RuntimeFeatureNotReported"}
			if feature.reports.ReportedSupportedNodes > 0 {
				resource.Status, resource.Reason = "Observed", "RuntimeFeatureReported"
				if feature.reports.ReportedUnsupportedNodes+feature.reports.UnknownNodes > 0 {
					resource.Reason = "PartialFeatureCoverage"
				}
			} else if feature.reports.ReportedUnsupportedNodes > 0 {
				resource.Reason = "RuntimeFeatureReportedUnsupported"
			}
			add(feature.name, feature.title, feature.category, feature.description, &resource)
		}
	}
	for _, storage := range status.StorageClasses {
		name, title, description := "UnclassifiedStorage", "Unclassified storage", "The Kubernetes volume interface has not been classified."
		switch storage.Type {
		case "Block":
			name, title, description = "BlockStorage", "Raw block volumes", "PVC volumeMode Block exposes a raw device; no filesystem, sharing or durability is implied."
		case "Filesystem":
			name, title, description = "Filesystem", "Filesystem volumes", "Volumes mount into containers as directories; full POSIX semantics, sharing and durability are not verified."
		}
		add(name, title, "Storage", description, &v1.CapabilityReference{Kind: "StorageClass", Name: storage.Name, Status: storage.Status, Source: storage.Source, Reason: storage.Reason})
	}
	if status.NetworkPolicy != nil {
		const description = "A configured policy engine can apply Pod network policies; packet enforcement has not been verified."
		add("NetworkPolicy", "Pod network policies", "Networking", description, nil)
		for _, provider := range status.NetworkPolicy.Providers {
			state := provider.Status
			if state == "Supported" {
				state = "Observed"
			}
			add("NetworkPolicy", "Pod network policies", "Networking", description,
				&v1.CapabilityReference{Kind: "DaemonSet", Name: provider.Name, Namespace: provider.Namespace, Status: state, Source: provider.Source, Reason: provider.Reason})
		}
	}
	result := make([]v1.CapabilityObservation, 0, len(groups))
	for _, group := range groups {
		sort.Slice(group.Resources, func(i, j int) bool {
			a, b := group.Resources[i], group.Resources[j]
			return a.Kind+"/"+a.Namespace+"/"+a.Name < b.Kind+"/"+b.Namespace+"/"+b.Name
		})
		result = append(result, *group)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
