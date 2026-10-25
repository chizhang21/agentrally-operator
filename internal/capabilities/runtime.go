package capabilities

import (
	"context"
	"sort"
	"strings"

	v1 "arally/operator/api/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func readyLinuxNode(n corev1.Node) bool {
	if n.DeletionTimestamp != nil || n.Spec.Unschedulable || n.Labels[corev1.LabelOSStable] != "linux" {
		return false
	}
	for _, condition := range n.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func matchesRuntimeNode(n corev1.Node, r *nodev1.RuntimeClass) bool {
	if !readyLinuxNode(n) {
		return false
	}
	var tolerations []corev1.Toleration
	if r != nil && r.Scheduling != nil {
		for key, value := range r.Scheduling.NodeSelector {
			if observed, present := n.Labels[key]; !present || observed != value {
				return false
			}
		}
		tolerations = r.Scheduling.Tolerations
	}
	// Kubelet adds the standard not-ready/unreachable tolerations, but other
	// blocking taints must be tolerated by the selected RuntimeClass.
	for _, taint := range n.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if taint.Key == "node.kubernetes.io/not-ready" || taint.Key == "node.kubernetes.io/unreachable" {
			continue
		}
		tolerated := false
		for _, t := range tolerations {
			// Cluster feature gates are not observed; preserve Equal/Exists matching.
			if t.ToleratesTaint(logr.Discard(), &taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

func discoverRuntimes(ctx context.Context, reader client.Reader, status *v1.ClusterCapabilitiesStatus) error {
	nodes := &corev1.NodeList{}
	if err := reader.List(ctx, nodes); err != nil {
		return err
	}
	classes := &nodev1.RuntimeClassList{}
	if err := reader.List(ctx, classes); err != nil {
		return err
	}
	baseline := v1.RuntimeObservation{Name: "default", Family: "unknown", Category: "Unknown", ImageContract: "Unknown", Status: "Unknown", Reason: "DefaultRuntimeUnclassified", Source: "NativeAPI"}
	indexed, handlers := indexRuntimeNodes(nodes.Items)
	status.RuntimeHandlers = handlers
	observeRuntimeNodes(&baseline, indexed, nil)
	type criVersion struct{ implementation, version string }
	versions := map[criVersion]int32{}
	for _, entry := range indexed {
		n := entry.node
		if version := n.Status.NodeInfo.ContainerRuntimeVersion; version != "" {
			parts := strings.SplitN(version, "://", 2)
			key := criVersion{implementation: parts[0]}
			if len(parts) == 2 {
				key.version = parts[1]
			}
			versions[key]++
		}
	}
	if baseline.EligibleNodes == 0 {
		baseline.Status = "Unknown"
		baseline.Reason = "NoEligibleNodes"
	}
	status.DefaultRuntime = &baseline
	keys := make([]criVersion, 0, len(versions))
	for key := range versions {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].implementation != keys[j].implementation {
			return keys[i].implementation < keys[j].implementation
		}
		return keys[i].version < keys[j].version
	})
	for _, key := range keys {
		status.NodeRuntimes = append(status.NodeRuntimes, v1.NodeRuntimeObservation{Implementation: key.implementation, Version: key.version, Nodes: versions[key]})
	}
	sort.Slice(classes.Items, func(i, j int) bool { return classes.Items[i].Name < classes.Items[j].Name })
	for _, r := range classes.Items {
		if r.DeletionTimestamp != nil {
			continue
		}
		c := describeRuntime(r)
		observeRuntimeNodes(&c, indexed, &r)
		runtimeEvidenceStatus(&c)
		status.Runtimes = append(status.Runtimes, c)
	}
	return nil
}
