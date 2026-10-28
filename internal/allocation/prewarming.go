package allocation

import (
	"context"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SelectDefinition uses the same immutable key as instance allocation, without
// creating a placeholder Instance or bypassing the Group's spare budget.
func (m *Manager) SelectDefinition(ctx context.Context, definition *v1.AgentDefinition, group *v1.AgentGroup, config Configuration, revision string) (*Selection, error) {
	source := "definition/" + definition.Name
	poolName, templateName := automaticNames(group, source, revision, config.Service)
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	pool.SetName(poolName)
	pool.SetNamespace(definition.Namespace)
	err := m.reader().Get(ctx, client.ObjectKeyFromObject(pool), pool)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil {
		if !managedByGroup(pool, group) || pool.GetLabels()[definitionUIDLabel] != string(definition.UID) {
			return nil, poolDiagnostic("PoolOwnershipConflict", "Proactive pool name belongs to another configuration owner")
		}
		name, refErr := sandbox.PoolTemplateName(pool)
		if pool.GetDeletionTimestamp() != nil || refErr != nil || name != templateName || pool.GetAnnotations()["arally.io/source-template"] != source {
			return nil, poolDiagnostic("PoolTemplateMismatch", "Proactive pool is terminating or differs from the selected configuration")
		}
	}
	return &Selection{PoolName: poolName, TemplateName: templateName, group: group, pool: pool,
		exists: err == nil, source: source, namespace: definition.Namespace, revision: revision, proactive: true}, nil
}

// ProtectDefinition must follow persistence of the selection. ResourceVersion
// changes even when a cleared selection resumes the same generation/fingerprint.
func (s *Selection) ProtectDefinition(definition *v1.AgentDefinition) {
	s.allocationToken = string(definition.UID) + "/" + definition.ResourceVersion
}

// SamePreparationSelection excludes capacity feedback so status observations do
// not retrigger Group budget inventories or invalidate a stable selection.
func SamePreparationSelection(a, b *v1.DefinitionPrewarmingStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	left, right := *a, *b
	left.DesiredReplicas, left.ReadyReplicas = 0, 0
	right.DesiredReplicas, right.ReadyReplicas = 0, 0
	return left == right
}

func activePreparation(definition *v1.AgentDefinition, group *v1.AgentGroup) *v1.DefinitionPrewarmingStatus {
	if definition == nil || definition.DeletionTimestamp != nil || group.DeletionTimestamp != nil ||
		group.Spec.Prewarming == nil || group.Spec.AllocationPaused || definition.Spec.Prewarming == nil || definition.Spec.Prewarming.Replicas <= 0 {
		return nil
	}
	publication := meta.FindStatusCondition(definition.Status.Conditions, "Ready")
	if definition.Status.ObservedGeneration != definition.Generation || publication == nil || publication.Status != metav1.ConditionTrue || publication.ObservedGeneration != definition.Generation {
		return nil
	}
	state := definition.Status.Prewarming
	if state == nil || state.ObservedGeneration != definition.Generation || state.Revision != definition.Status.CurrentRevision ||
		state.Revision == "" || state.ConfigurationRevision == "" || state.PoolName == "" || state.TemplateName == "" ||
		state.RequestedReplicas != definition.Spec.Prewarming.Replicas {
		return nil
	}
	return state
}

func poolDemand(pool *unstructured.Unstructured, group *v1.AgentGroup, definitions map[string]*v1.AgentDefinition) (int64, bool) {
	definition := definitions[pool.GetLabels()[definitionUIDLabel]]
	proactive := pool.GetAnnotations()[proactiveAnnotation] == "true" || definition != nil && definition.Spec.Prewarming != nil
	if proactive {
		state := activePreparation(definition, group)
		name, err := sandbox.PoolTemplateName(pool)
		if state == nil || state.PoolName != pool.GetName() || state.TemplateName != name || err != nil ||
			pool.GetAnnotations()["arally.io/source-template"] != "definition/"+definition.Name {
			return 0, true
		}
		return min(int64(state.RequestedReplicas), int64(group.Spec.Prewarming.Replicas)), true
	}
	if pool.GetAnnotations()[existingWorkspaceAnnotation] == "true" || group.DeletionTimestamp != nil {
		return 0, true
	}
	if group.Spec.Prewarming != nil {
		return int64(group.Spec.Prewarming.Replicas), true
	}
	return 0, false // No new policy: preserve manually managed replica counts.
}
