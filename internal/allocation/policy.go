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
func (m *Manager) reconcilePoolUsage(ctx context.Context, pool *unstructured.Unstructured, group *v1.AgentGroup, used bool) error {
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
	return m.patchPool(ctx, pool, base)
}

// Instance allocation applies per-pool policy. Proactive pools
// and total budgets are exclusively scaled by Group reconciliation.
func (m *Manager) reconcilePoolPolicy(ctx context.Context, pool *unstructured.Unstructured, group *v1.AgentGroup, used bool) error {
	if err := m.reconcilePoolUsage(ctx, pool, group, used); err != nil {
		return err
	}
	if pool.GetDeletionTimestamp() != nil || pool.GetAnnotations()[proactiveAnnotation] == "true" {
		return nil
	}
	policy := group.Spec.Prewarming
	if policy != nil && policy.MaxTotalReplicas != nil {
		return nil
	}
	if policy == nil && pool.GetAnnotations()[existingWorkspaceAnnotation] != "true" {
		return nil
	}
	replicas := int64(0)
	if policy != nil && pool.GetAnnotations()[existingWorkspaceAnnotation] != "true" {
		replicas = int64(policy.Replicas)
	}
	base := pool.DeepCopy()
	if err := unstructured.SetNestedField(pool.Object, replicas, "spec", "replicas"); err != nil {
		return err
	}
	return m.patchPool(ctx, pool, base)
}

func (m *Manager) patchPool(ctx context.Context, pool, base *unstructured.Unstructured) error {
	if equality.Semantic.DeepEqual(base.Object, pool.Object) {
		return nil
	}
	return m.Client.Patch(ctx, pool, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (m *Manager) reconcileSpareBudget(ctx context.Context, group *v1.AgentGroup, pools []unstructured.Unstructured, definitions map[string]*v1.AgentDefinition) error {
	var managed, eligible []*unstructured.Unstructured
	targets := map[string]int64{}
	budget := int64(0)
	limited := group.Spec.Prewarming != nil && group.Spec.Prewarming.MaxTotalReplicas != nil
	if limited {
		budget = int64(*group.Spec.Prewarming.MaxTotalReplicas)
	}
	for i := range pools {
		pool := &pools[i]
		if !managedByGroup(pool, group) {
			continue
		}
		if pool.GetDeletionTimestamp() != nil {
			if limited {
				replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
				budget -= max(replicas, 0)
			}
			continue
		}
		demand, controlled := poolDemand(pool, group, definitions)
		if !controlled {
			continue
		}
		managed = append(managed, pool)
		targets[pool.GetName()] = demand
		if demand > 0 {
			eligible = append(eligible, pool)
		}
	}
	if limited {
		// Fill small requests first, then share the remaining budget. Equal
		// demands use stable names for remainder assignment; work is O(n log n).
		sort.Slice(eligible, func(i, j int) bool {
			a, b := eligible[i].GetName(), eligible[j].GetName()
			if targets[a] != targets[b] {
				return targets[a] < targets[b]
			}
			return a < b
		})
		budget = max(budget, 0)
		for i, pool := range eligible {
			left := int64(len(eligible) - i)
			share := budget / left
			if budget%left > 0 {
				share++
			}
			target := min(targets[pool.GetName()], share)
			targets[pool.GetName()] = target
			budget -= target
		}
	}
	// Reductions precede increases. Optimistic locking also rejects inventories
	// invalidated by an in-flight allocation or reactivated Definition selection.
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
