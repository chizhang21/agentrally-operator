package controller

import (
	v1 "agentrally/operator/api/v1alpha1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SummarizeInstances counts current conditions; readiness and availability overlap.
func SummarizeInstances(instances []v1.AgentInstance) v1.AgentInstanceSummary {
	result := v1.AgentInstanceSummary{Total: int32(len(instances))}
	for i := range instances {
		instance := &instances[i]
		if instance.Status.Lifecycle != nil && instance.Status.Lifecycle.ExpiredAt != nil {
			result.Expired++
			continue
		}
		if instance.DeletionTimestamp != nil {
			continue
		}
		current := func(kind string) bool {
			condition := meta.FindStatusCondition(instance.Status.Conditions, kind)
			return condition != nil && condition.ObservedGeneration == instance.Generation && condition.Status == metav1.ConditionTrue
		}
		ready, suspended := current(ConditionReady), current(ConditionSuspended)
		if ready {
			result.Ready++
		}
		if current(ConditionAvailable) {
			result.Available++
		}
		if suspended {
			result.Suspended++
		}
		if !ready && !suspended {
			result.Pending++
		}
	}
	return result
}
