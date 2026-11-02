package api_test

import (
	"reflect"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/testutil"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestAllManifestsSurviveAdmissionAndPruning(t *testing.T) {
	checks := map[string]func(*testing.T, *unstructured.Unstructured){}
	counts := map[string]int{}
	for _, plural := range []string{"agentgroups", "agentdefinitions", "agentinstances", "clustercapabilities"} {
		crd := renderedCRD(t, plural)
		schema := newAdmissionSchema(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema, true)
		checks[crd.Spec.Names.Kind] = func(t *testing.T, object *unstructured.Unstructured) {
			t.Helper()
			errors := apivalidation.ValidateObjectMetaAccessor(object, crd.Spec.Scope == apiextensionsv1.NamespaceScoped, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
			before := runtime.DeepCopyJSON(object.Object)
			pruning.Prune(object.Object, schema.structural, true)
			if !reflect.DeepEqual(before, object.Object) {
				t.Fatal("example contains unsupported fields lost at admission")
			}
			defaulting.Default(object.Object, schema.structural)
			errors = append(errors, schema.validate(object.Object, nil)...)
			if len(errors) != 0 {
				t.Fatalf("example fails CRD admission: %v", errors.ToAggregate())
			}
		}
	}
	for _, example := range testutil.ExampleManifests(t) {
		object := example.Object
		if object.GroupVersionKind().Group != v1.GroupVersion.Group {
			continue
		}
		t.Run(example.Path, func(t *testing.T) {
			check, known := checks[object.GetKind()]
			if !known || object.GetAPIVersion() != v1.GroupVersion.String() {
				t.Fatalf("example uses an unsupported AgentRally API: %s", object.GroupVersionKind())
			}
			check(t, object)
			counts[object.GetKind()]++
		})
	}
	for kind := range checks {
		if counts[kind] == 0 {
			t.Errorf("no %s examples were checked", kind)
		}
		t.Logf("validated %d %s examples", counts[kind], kind)
	}
}

func TestManifestReferencesResolveWithinTheSameGroup(t *testing.T) {
	type identity struct{ kind, namespace, name string }
	index := map[identity]*unstructured.Unstructured{}
	examples := testutil.ExampleManifests(t)
	for _, example := range examples {
		object := example.Object
		index[identity{object.GetKind(), object.GetNamespace(), object.GetName()}] = object
	}
	for _, example := range examples {
		object := example.Object
		if object.GetKind() != "AgentDefinition" && object.GetKind() != "AgentInstance" {
			continue
		}
		t.Run(example.Path, func(t *testing.T) {
			groupName, _, err := unstructured.NestedString(object.Object, "spec", "agentGroupRef", "name")
			if err != nil {
				t.Fatal(err)
			}
			groupObject := index[identity{kind: "AgentGroup", name: groupName}]
			if groupObject == nil {
				t.Fatalf("example's AgentGroup %q is missing", groupName)
			}
			group := &v1.AgentGroup{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(groupObject.Object, group); err != nil {
				t.Fatal(err)
			}
			if object.GetNamespace() != group.NamespaceName() {
				t.Fatal("example is outside its Group namespace")
			}
			kind, fieldName := "AgentDefinition", "definitionRef"
			if object.GetKind() == "AgentDefinition" {
				kind, fieldName = "SandboxTemplate", "templateRef"
			}
			name, found, err := unstructured.NestedString(object.Object, "spec", fieldName, "name")
			if err != nil {
				t.Fatal(err)
			}
			if !found && object.GetKind() == "AgentDefinition" {
				return // Built-in definitions do not reference a native source template.
			}
			dependency := index[identity{kind, object.GetNamespace(), name}]
			if dependency == nil {
				t.Fatalf("example's %s %q is missing from its namespace", kind, name)
			}
			if kind == "AgentDefinition" {
				definitionGroup, _, err := unstructured.NestedString(dependency.Object, "spec", "agentGroupRef", "name")
				if err != nil || definitionGroup != groupName {
					t.Fatal("example references a definition in another Group", err)
				}
			}
		})
	}
}
