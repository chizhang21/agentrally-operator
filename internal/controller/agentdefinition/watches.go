package agentdefinition

import (
	"context"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const definitionDependencyIndex = "agentrally.definitionDependency"

func definitionDependencyKeys(object client.Object) []string {
	definition := object.(*v1.AgentDefinition)
	var keys []string
	if name := definition.Spec.AgentGroupRef.Name; name != "" {
		keys = append(keys, "AgentGroup/"+name)
	}
	if ref := definition.Spec.TemplateRef; ref != nil && ref.Name != "" {
		keys = append(keys, "SandboxTemplate/"+ref.Name)
	}
	return keys
}

// Event handlers select candidates from the cache. Reconcile verifies current
// dependency state and namespace ownership through its authoritative reader.
func (r *Reconciler) mapDependency(ctx context.Context, object client.Object) []reconcile.Request {
	var key string
	switch dependency := object.(type) {
	case *v1.AgentGroup:
		key = "AgentGroup/" + dependency.Name
	case *unstructured.Unstructured:
		if dependency.GroupVersionKind() != sandbox.ExtensionsGroupVersion.WithKind("SandboxTemplate") {
			return nil
		}
		key = "SandboxTemplate/" + dependency.GetName()
	default:
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	options := []client.ListOption{client.MatchingFields{definitionDependencyIndex: key}}
	if object.GetNamespace() != "" {
		options = append(options, client.InNamespace(object.GetNamespace()))
	}
	definitions := &v1.AgentDefinitionList{}
	if err := r.List(ctx, definitions, options...); err != nil {
		ctrl.LoggerFrom(ctx).Error(diagnostics.SafeError(err), "Unable to map definition dependency")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(definitions.Items))
	for i := range definitions.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&definitions.Items[i])})
	}
	return requests
}
