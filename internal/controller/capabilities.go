package controller

import (
	v1 "agentrally/operator/api/v1alpha1"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// CapabilityChanges ignores audit timestamps and informational node inventories.
// Periodic reconciliation still checks freshness and recovers missed events.
func CapabilityChanges() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
		before, beforeOK := update.ObjectOld.(*v1.ClusterCapabilities)
		after, afterOK := update.ObjectNew.(*v1.ClusterCapabilities)
		if !beforeOK || !afterOK {
			return true
		}
		left, right := before.Status.DeepCopy(), after.Status.DeepCopy()
		left.ObservedAt, right.ObservedAt = nil, nil
		left.Sections, right.Sections = nil, nil
		left.Platforms, right.Platforms = nil, nil
		left.ExtendedResources, right.ExtendedResources = nil, nil
		return before.Generation != after.Generation || !equality.Semantic.DeepEqual(left, right)
	}}
}
