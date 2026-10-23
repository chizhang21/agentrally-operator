package agentinstance

import (
	"context"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/definition"
	"arally/operator/internal/diagnostics"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func loadDefinitionRevision(ctx context.Context, reader client.Reader, instance *v1.AgentInstance) (*definition.Revision, error) {
	object := &v1.AgentDefinition{}
	name := instance.Spec.DefinitionRef.Name
	if name == "" {
		return nil, diagnostics.New("InvalidConfiguration", "definitionRef.name is required", nil)
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: name}, object); err != nil {
		return nil, diagnostics.ReferenceError("AgentDefinition", instance.Namespace, name, err)
	}
	if object.Spec.AgentGroupRef.Name != instance.Spec.AgentGroupRef.Name {
		return nil, diagnostics.New("DefinitionGroupMismatch", "Definition and instance must belong to the same AgentGroup", nil)
	}
	accepted := instance.Status.Definition
	if accepted != nil && (accepted.Name != name || accepted.UID != object.UID) {
		return nil, diagnostics.New("DefinitionIdentityChanged", "The definition was replaced; create a new instance to accept its identity", nil)
	}
	if object.DeletionTimestamp != nil && (accepted == nil || accepted.Revision == "") {
		return nil, diagnostics.New("DependencyTerminating", "The referenced definition is being deleted", nil)
	}
	revision := instance.Spec.Revision
	if revision == "" && accepted != nil {
		revision = accepted.Revision
	}
	if revision == "" {
		if object.DeletionTimestamp != nil {
			return nil, diagnostics.New("DependencyTerminating", "The referenced definition is being deleted", nil)
		}
		ready := meta.FindStatusCondition(object.Status.Conditions, "Ready")
		if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != object.Generation || object.Status.CurrentRevision == "" {
			if ready != nil && ready.Status == metav1.ConditionFalse {
				return nil, diagnostics.New(ready.Reason, ready.Message, nil)
			}
			return nil, diagnostics.New("DefinitionNotReady", "Waiting for the definition to publish its configuration", nil)
		}
		revision = object.Status.CurrentRevision
	}
	return definition.Read(ctx, reader, object, revision)
}

func (r *Reconciler) bindDefinition(ctx context.Context, instance *v1.AgentInstance, binding *v1.DefinitionBinding) error {
	if current := instance.Status.Definition; current != nil {
		if current.Name != binding.Name || current.UID != binding.UID {
			return diagnostics.New("DefinitionIdentityChanged", "The definition identity changed", nil)
		}
		if current.Revision != "" {
			return nil
		}
	}
	base := instance.DeepCopy()
	instance.Status.Definition = binding.DeepCopy()
	return r.patchStatus(ctx, instance, base)
}
