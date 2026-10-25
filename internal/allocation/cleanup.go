package allocation

import (
	"context"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Prune removes unused AgentRally-generated pools and snapshots, returning whether cleanup is pending.
// Read current references before pruning. Pool selection is persisted before its
// creation, so an in-flight AgentInstance protects a newly created shared pool.
// Native Claims also protect pools if an external caller uses managed capacity.
func (m *Manager) Prune(ctx context.Context, group *v1.AgentGroup) (bool, error) {
	return m.prune(ctx, group, "")
}

// RetireDefinition drains unused capacity for a deleting definition, preserving native Claim references.
func (m *Manager) RetireDefinition(ctx context.Context, group *v1.AgentGroup, uid types.UID) (bool, error) {
	return m.prune(ctx, group, uid)
}

func (m *Manager) prune(ctx context.Context, group *v1.AgentGroup, definitionUID types.UID) (bool, error) {
	reader := m.Reader
	if reader == nil {
		reader = m.Client
	}
	workloads := &v1.AgentInstanceList{}
	claims, pools, templates := sandbox.ExtensionList("SandboxClaim"), sandbox.ExtensionList("SandboxWarmPool"), sandbox.ExtensionList("SandboxTemplate")
	for _, list := range []*unstructured.UnstructuredList{pools, templates} {
		if err := reader.List(ctx, list, client.InNamespace(group.NamespaceName())); err != nil {
			return false, err
		}
	}
	// Read selection status after resource inventories: any listed new resource
	// was created only after its protecting selection was persisted.
	if err := reader.List(ctx, workloads, client.InNamespace(group.NamespaceName())); err != nil {
		return false, err
	}
	if err := reader.List(ctx, claims, client.InNamespace(group.NamespaceName())); err != nil {
		return false, err
	}
	used := map[string]bool{}
	remainingTemplates := map[string]bool{}
	for _, w := range workloads.Items {
		if w.DeletionTimestamp == nil && (w.Status.Lifecycle == nil || w.Status.Lifecycle.ExpiredAt == nil) && w.Status.Provisioning != nil && w.Status.Provisioning.TimedOutAt == nil {
			used[w.Status.Provisioning.PoolName] = true
			remainingTemplates[w.Status.Provisioning.TemplateName] = true
		}
	}
	for _, claim := range claims.Items {
		name, _ := sandbox.ClaimPoolName(&claim)
		used[name] = true
	}
	if definitionUID == "" {
		// Keep the Group as the sole writer of the shared budget. Definition
		// retirement only drains its own resources, without scaling siblings.
		if err := m.reconcileSpareBudget(ctx, group, pools.Items); err != nil {
			return false, err
		}
	}
	pending := false
	for i := range pools.Items {
		pool := &pools.Items[i]
		name, _ := sandbox.PoolTemplateName(pool)
		remainingTemplates[name] = true // keep template until foreground pool cleanup completes.
		if !managedByGroup(pool, group) || (definitionUID != "" && pool.GetLabels()[definitionUIDLabel] != string(definitionUID)) {
			continue
		}
		if err := m.reconcilePoolPolicy(ctx, pool, group, used[pool.GetName()]); err != nil {
			return false, err
		}
		if used[pool.GetName()] {
			if definitionUID != "" {
				pending = true
			}
			continue
		}
		if definitionUID == "" && m.retainIdlePool(pool, group) {
			continue
		}
		pending = true
		if err := m.deleteAllocationResource(ctx, pool); err != nil {
			return false, err
		}
	}
	for i := range templates.Items {
		template := &templates.Items[i]
		if !managedByGroup(template, group) || (definitionUID != "" && template.GetLabels()[definitionUIDLabel] != string(definitionUID)) || remainingTemplates[template.GetName()] {
			continue
		}
		pending = true
		if err := m.deleteAllocationResource(ctx, template); err != nil {
			return false, err
		}
	}
	return pending, nil
}

func (m *Manager) deleteAllocationResource(ctx context.Context, object *unstructured.Unstructured) error {
	if object.GetDeletionTimestamp() != nil {
		return nil
	}
	err := m.Client.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationForeground), &client.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: ptr.To(object.GetUID()), ResourceVersion: ptr.To(object.GetResourceVersion()),
	}})
	if apierrors.IsConflict(err) {
		// A concurrent allocation or upstream status write invalidated our
		// snapshot. Re-evaluate references on the next reconciliation.
		return nil
	}
	return client.IgnoreNotFound(err)
}
