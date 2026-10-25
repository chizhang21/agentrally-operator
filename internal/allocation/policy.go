package allocation

import (
	"context"
	"sort"
	"time"

	v1 "agentrally/operator/api/v1alpha1"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const unusedSinceAnnotation = "arally.io/unused-since"
const existingWorkspaceAnnotation = "arally.io/existing-workspace"

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// Reconcile only the declared replica policy and idle anchor. Nil policy leaves
// manual scaling alone; an external pool is never passed to this method.
func (m *Manager) reconcilePoolPolicy(ctx context.Context, pool *unstructured.Unstructured, group *v1.AgentGroup, used bool) error {
	if pool.GetDeletionTimestamp() != nil {
		return nil
	}
	base := pool.DeepCopy()
	annotations := pool.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if used {
		delete(annotations, unusedSinceAnnotation)
	} else if policy := group.Spec.Prewarming; policy != nil && policy.IdleTTLSeconds > 0 && group.DeletionTimestamp == nil {
		if _, err := time.Parse(time.RFC3339, annotations[unusedSinceAnnotation]); err != nil {
			annotations[unusedSinceAnnotation] = m.now().Format(time.RFC3339)
		}
	}
	pool.SetAnnotations(annotations)
	if policy := group.Spec.Prewarming; policy != nil || pool.GetAnnotations()[existingWorkspaceAnnotation] == "true" {
		replicas := int64(0)
		if policy != nil && pool.GetAnnotations()[existingWorkspaceAnnotation] != "true" {
			if policy.MaxTotalReplicas != nil {
				// Only Group reconciliation distributes a total budget. Allocation
				// must not independently refill a pool past that shared decision.
				return m.patchPool(ctx, pool, base)
			}
			replicas = int64(policy.Replicas)
		}
		if err := unstructured.SetNestedField(pool.Object, replicas, "spec", "replicas"); err != nil {
			return err
		}
	}
	return m.patchPool(ctx, pool, base)
}

func (m *Manager) patchPool(ctx context.Context, pool, base *unstructured.Unstructured) error {
	if equality.Semantic.DeepEqual(base.Object, pool.Object) {
		return nil
	}
	return m.Client.Patch(ctx, pool, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (m *Manager) reconcileSpareBudget(ctx context.Context, group *v1.AgentGroup, pools []unstructured.Unstructured) error {
	policy := group.Spec.Prewarming
	if policy == nil || policy.MaxTotalReplicas == nil {
		return nil
	}
	var managed, eligible []*unstructured.Unstructured
	budget := int64(*policy.MaxTotalReplicas)
	for i := range pools {
		pool := &pools[i]
		if !managedByGroup(pool, group) {
			continue
		}
		if pool.GetDeletionTimestamp() != nil {
			// Foreground cleanup may still own spare Pods. Do not redistribute
			// their target budget until the terminating pool has disappeared.
			replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
			budget -= replicas
			continue
		}
		managed = append(managed, pool)
		if pool.GetAnnotations()[existingWorkspaceAnnotation] != "true" {
			eligible = append(eligible, pool)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].GetName() < eligible[j].GetName() })
	targets := map[string]int64{}
	if len(eligible) > 0 {
		if budget < 0 {
			budget = 0
		}
		share, extra := budget/int64(len(eligible)), budget%int64(len(eligible))
		for i, pool := range eligible {
			target := share
			if int64(i) < extra {
				target++
			}
			if target > int64(policy.Replicas) {
				target = int64(policy.Replicas)
			}
			targets[pool.GetName()] = target
		}
	}
	// Reduce before increasing other pools so redistribution does not introduce
	// a transient desired-replica overshoot. Concurrent native edits still obey
	// optimistic locking; ResourceQuota supplies hard resource admission limits.
	for _, reducing := range []bool{true, false} {
		for _, pool := range managed {
			current, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
			target := targets[pool.GetName()]
			if target == current || (target < current) != reducing {
				continue
			}
			base := pool.DeepCopy()
			if err := unstructured.SetNestedField(pool.Object, target, "spec", "replicas"); err != nil {
				return err
			}
			if err := m.patchPool(ctx, pool, base); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) retainIdlePool(pool *unstructured.Unstructured, group *v1.AgentGroup) bool {
	policy := group.Spec.Prewarming
	if group.DeletionTimestamp != nil || policy == nil || policy.IdleTTLSeconds == 0 {
		return false
	}
	since, err := time.Parse(time.RFC3339, pool.GetAnnotations()[unusedSinceAnnotation])
	return err == nil && m.now().Before(since.Add(time.Duration(policy.IdleTTLSeconds)*time.Second))
}
