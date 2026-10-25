package capabilities

import (
	"sort"

	v1 "arally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
)

type runtimeNode struct {
	node     *corev1.Node
	handlers map[string]*corev1.NodeRuntimeHandlerFeatures
}

// Index once per scan. Raw reports include nodes excluded by class scheduling;
// class-level coverage below uses only that class's existing candidate checks.
func indexRuntimeNodes(nodes []corev1.Node) ([]runtimeNode, []v1.NodeRuntimeHandlerObservation) {
	indexed := make([]runtimeNode, 0, len(nodes))
	inventory := map[string]*v1.NodeRuntimeHandlerObservation{}
	for i := range nodes {
		node := &nodes[i]
		if node.DeletionTimestamp != nil {
			continue
		}
		entry := runtimeNode{node: node, handlers: make(map[string]*corev1.NodeRuntimeHandlerFeatures, len(node.Status.RuntimeHandlers))}
		for _, handler := range node.Status.RuntimeHandlers {
			entry.handlers[handler.Name] = handler.Features
		}
		indexed = append(indexed, entry)
		for name, features := range entry.handlers {
			report := inventory[name]
			if report == nil {
				report = &v1.NodeRuntimeHandlerObservation{Name: name}
				inventory[name] = report
			}
			report.ReportedNodes++
			observeRuntimeFeatures(&report.Features, features)
		}
	}
	var reports []v1.NodeRuntimeHandlerObservation
	for _, report := range inventory {
		reports = append(reports, *report)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Name < reports[j].Name })
	return indexed, reports
}

func observeRuntimeNodes(c *v1.RuntimeObservation, nodes []runtimeNode, class *nodev1.RuntimeClass) {
	c.HandlerEvidence = &v1.RuntimeHandlerEvidence{}
	c.Features = &v1.RuntimeFeaturesObservation{}
	for _, entry := range nodes {
		if !matchesRuntimeNode(*entry.node, class) {
			continue
		}
		c.EligibleNodes++
		features, reported := entry.handlers[c.Handler]
		switch {
		case reported:
			c.HandlerEvidence.ReportedNodes++
		case len(entry.handlers) == 0:
			c.HandlerEvidence.NoInventoryNodes++
		default:
			c.HandlerEvidence.MissingFromReportedInventoryNodes++
		}
		observeRuntimeFeatures(c.Features, features)
	}
}

func observeRuntimeFeatures(observation *v1.RuntimeFeaturesObservation, features *corev1.NodeRuntimeHandlerFeatures) {
	var userNamespaces, recursiveReadOnly *bool
	if features != nil {
		userNamespaces, recursiveReadOnly = features.UserNamespaces, features.RecursiveReadOnlyMounts
	}
	observeRuntimeFeature(&observation.UserNamespaces, userNamespaces)
	observeRuntimeFeature(&observation.RecursiveReadOnlyMounts, recursiveReadOnly)
}

func observeRuntimeFeature(observation *v1.RuntimeFeatureObservation, supported *bool) {
	switch {
	case supported == nil:
		observation.UnknownNodes++
	case *supported:
		observation.ReportedSupportedNodes++
	default:
		observation.ReportedUnsupportedNodes++
	}
}
