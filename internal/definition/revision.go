// Package definition publishes and reads immutable agent configuration revisions.
package definition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/templates"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	SpecAnnotation     = "arally.io/definition-spec"
	RevisionAnnotation = "arally.io/definition-revision"
	UIDLabel           = "arally.io/definition-uid"
)

type Revision struct {
	Spec          v1.AgentDefinitionSpec
	Template      *unstructured.Unstructured
	ID            string
	DefinitionUID types.UID
}

func Name(uid types.UID, revision string) string {
	sum := sha256.Sum256([]byte(string(uid) + "/" + revision))
	return "arally-definition-" + hex.EncodeToString(sum[:16])
}

// Publish captures native source configuration without creating a Sandbox or Pod.
// External credentials remain references; publication never copies Secret data.
func Publish(ctx context.Context, c client.Client, reader client.Reader, scheme *runtime.Scheme, object *v1.AgentDefinition) (*Revision, error) {
	if object.UID == "" || object.Namespace == "" || (object.Spec.Template != "") == (object.Spec.TemplateRef != nil) {
		return nil, diagnostics.New("InvalidConfiguration", "Definition requires identity and exactly one template source", nil)
	}
	template := sandbox.ExtensionObject("SandboxTemplate")
	if object.Spec.Template != "" {
		pod, err := templates.Resolve(object.Spec.Template, object.Spec.Container)
		if err != nil {
			return nil, err
		}
		fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
		if err != nil {
			return nil, err
		}
		template.Object["spec"] = map[string]interface{}{"podTemplate": fields, "service": true, "networkPolicyManagement": "Unmanaged"}
	} else {
		if len(validation.IsDNS1123Subdomain(object.Spec.TemplateRef.Name)) != 0 {
			return nil, diagnostics.New("InvalidConfiguration", "templateRef.name must be a Kubernetes resource name", nil)
		}
		if object.Spec.Container != nil {
			return nil, diagnostics.New("InvalidConfiguration", "Custom definitions use templateRef instead of container settings", nil)
		}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: object.Namespace, Name: object.Spec.TemplateRef.Name}, template); err != nil {
			if meta.IsNoMatchError(err) {
				return nil, diagnostics.New("TemplateAPIUnavailable", "Install the agent-sandbox Extensions API before publishing definitions", err)
			}
			return nil, diagnostics.ReferenceError("SandboxTemplate", object.Namespace, object.Spec.TemplateRef.Name, err)
		}
		if template.GetDeletionTimestamp() != nil {
			return nil, diagnostics.New("DependencyTerminating", "The definition source template is terminating", nil)
		}
		management, _, err := unstructured.NestedString(template.Object, "spec", "networkPolicyManagement")
		if err != nil || management != "Unmanaged" {
			return nil, diagnostics.New("TemplatePolicyUnsupported", "Definitions require networkPolicyManagement: Unmanaged", err)
		}
		policy, _, err := unstructured.NestedMap(template.Object, "spec", "networkPolicy")
		if err != nil || len(policy) != 0 {
			return nil, diagnostics.New("TemplatePolicyUnsupported", "AgentGroup owns networking; source networkPolicy is not supported", err)
		}
	}
	// Make upstream template defaults explicit before publication identity is
	// calculated; API admission must not change a revision's stored inputs.
	if err := unstructured.SetNestedField(template.Object, "Disallowed", "spec", "envVarsInjectionPolicy"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(template.Object, "Disallowed", "spec", "volumeClaimTemplatesPolicy"); err != nil {
		return nil, err
	}
	service, _, err := unstructured.NestedBool(template.Object, "spec", "service")
	if err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(template.Object, service, "spec", "service"); err != nil {
		return nil, err
	}
	spec, err := json.Marshal(object.Spec)
	if err != nil {
		return nil, err
	}
	id, err := revisionID(spec, template.Object["spec"])
	if err != nil {
		return nil, err
	}
	result := sandbox.ExtensionObject("SandboxTemplate")
	result.SetName(Name(object.UID, id))
	result.SetNamespace(object.Namespace)
	result.SetLabels(map[string]string{UIDLabel: string(object.UID)})
	result.SetAnnotations(map[string]string{SpecAnnotation: string(spec), RevisionAnnotation: id})
	result.Object["spec"] = runtime.DeepCopyJSONValue(template.Object["spec"])
	if err := ctrl.SetControllerReference(object, result, scheme); err != nil {
		return nil, err
	}
	existing := sandbox.ExtensionObject("SandboxTemplate")
	err = reader.Get(ctx, client.ObjectKeyFromObject(result), existing)
	if apierrors.IsNotFound(err) {
		err = c.Create(ctx, result)
		if err == nil {
			return &Revision{Spec: *object.Spec.DeepCopy(), Template: result, ID: id, DefinitionUID: object.UID}, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		err = reader.Get(ctx, client.ObjectKeyFromObject(result), existing)
	}
	if err != nil {
		return nil, err
	}
	if err := controller.CheckOwner(existing, object); err != nil {
		return nil, err
	}
	if existing.GetDeletionTimestamp() != nil || existing.GetAnnotations()[SpecAnnotation] != string(spec) || existing.GetAnnotations()[RevisionAnnotation] != id || !equality.Semantic.DeepEqual(existing.Object["spec"], result.Object["spec"]) {
		return nil, diagnostics.New("DefinitionRevisionConflict", "A published configuration revision has been changed or is terminating", nil)
	}
	result = existing
	return &Revision{Spec: *object.Spec.DeepCopy(), Template: result, ID: id, DefinitionUID: object.UID}, nil
}

func Read(ctx context.Context, reader client.Reader, object *v1.AgentDefinition, revision string) (*Revision, error) {
	template := sandbox.ExtensionObject("SandboxTemplate")
	if err := reader.Get(ctx, client.ObjectKey{Namespace: object.Namespace, Name: Name(object.UID, revision)}, template); err != nil {
		return nil, diagnostics.ReferenceError("DefinitionRevision", object.Namespace, revision, err)
	}
	if err := controller.CheckOwner(template, object); err != nil {
		return nil, err
	}
	if template.GetDeletionTimestamp() != nil || template.GetAnnotations()[RevisionAnnotation] != revision {
		return nil, diagnostics.New("DefinitionRevisionConflict", "The selected published revision is unavailable", nil)
	}
	var spec v1.AgentDefinitionSpec
	if err := json.Unmarshal([]byte(template.GetAnnotations()[SpecAnnotation]), &spec); err != nil {
		return nil, diagnostics.New("DefinitionRevisionConflict", "The published configuration cannot be decoded", err)
	}
	// Recompute the publication identity so metadata changes cannot redirect a
	// pinned instance to different inputs under the same revision name.
	id, err := revisionID(json.RawMessage(template.GetAnnotations()[SpecAnnotation]), template.Object["spec"])
	if err != nil {
		return nil, err
	}
	if id != revision {
		return nil, diagnostics.New("DefinitionRevisionConflict", "The published revision differs from its configuration identity", nil)
	}
	return &Revision{Spec: spec, Template: template, ID: revision, DefinitionUID: object.UID}, nil
}

func revisionID(spec json.RawMessage, native any) (string, error) {
	input, err := json.Marshal(struct {
		Definition json.RawMessage
		Native     any
	}{spec, native})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:]), nil
}
