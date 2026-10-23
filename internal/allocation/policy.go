package allocation

import (
	"context"
	"time"

	v1 "arally/operator/api/v1alpha1"

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
			replicas = int64(policy.Replicas)
		}
		if err := unstructured.SetNestedField(pool.Object, replicas, "spec", "replicas"); err != nil {
			return err
		}
	}
	if equality.Semantic.DeepEqual(base.Object, pool.Object) {
		return nil
	}
	return m.Client.Patch(ctx, pool, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (m *Manager) retainIdlePool(pool *unstructured.Unstructured, group *v1.AgentGroup) bool {
	policy := group.Spec.Prewarming
	if group.DeletionTimestamp != nil || policy == nil || policy.IdleTTLSeconds == 0 {
		return false
	}
	since, err := time.Parse(time.RFC3339, pool.GetAnnotations()[unusedSinceAnnotation])
	return err == nil && m.now().Before(since.Add(time.Duration(policy.IdleTTLSeconds)*time.Second))
}
