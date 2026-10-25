package packaging_test

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

type exampleResource struct {
	path   string
	object *unstructured.Unstructured
}

func exampleResources(t *testing.T) []exampleResource {
	t.Helper()
	const root = "../../examples"
	var examples []exampleResource
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for document := 1; ; document++ {
			object := &unstructured.Unstructured{}
			if err := decoder.Decode(object); err == io.EOF {
				return nil
			} else if err != nil {
				return fmt.Errorf("%s document %d: %w", path, document, err)
			}
			if len(object.Object) != 0 {
				examples = append(examples, exampleResource{path: fmt.Sprintf("%s/%d", relative, document), object: object})
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 0 {
		t.Fatal("example resources are missing")
	}
	return examples
}

func TestAllExamplesSurviveAdmissionAndPruning(t *testing.T) {
	checks := map[string]func(*testing.T, *unstructured.Unstructured){}
	counts := map[string]int{}
	for _, plural := range []string{"agentgroups", "agentdefinitions", "agentinstances", "clustercapabilities"} {
		crd := renderedCRD(t, plural)
		schema := &apiextensions.JSONSchemaProps{}
		if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, schema, nil); err != nil {
			t.Fatal(err)
		}
		structural, err := structuralschema.NewStructural(schema)
		if err != nil {
			t.Fatal(err)
		}
		validator, _, err := apiservervalidation.NewSchemaValidator(schema)
		if err != nil {
			t.Fatal(err)
		}
		cel := structuralcel.NewValidator(structural, true, 1_000_000)
		checks[crd.Spec.Names.Kind] = func(t *testing.T, object *unstructured.Unstructured) {
			t.Helper()
			errors := apivalidation.ValidateObjectMetaAccessor(object, crd.Spec.Scope == apiextensionsv1.NamespaceScoped, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
			before := runtime.DeepCopyJSON(object.Object)
			pruning.Prune(object.Object, structural, true)
			if !reflect.DeepEqual(before, object.Object) {
				t.Fatal("example contains unsupported fields lost at admission")
			}
			defaulting.Default(object.Object, structural)
			errors = append(errors, apiservervalidation.ValidateCustomResource(field.NewPath("resource"), object.Object, validator)...)
			if cel != nil {
				celErrors, _ := cel.Validate(t.Context(), field.NewPath("resource"), structural, object.Object, nil, 1_000_000)
				errors = append(errors, celErrors...)
			}
			if len(errors) != 0 {
				t.Fatalf("example fails CRD admission: %v", errors.ToAggregate())
			}
		}
	}
	for _, example := range exampleResources(t) {
		object := example.object
		if object.GroupVersionKind().Group != v1.GroupVersion.Group {
			continue
		}
		t.Run(example.path, func(t *testing.T) {
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

func TestExampleReferencesResolveWithinTheSameGroup(t *testing.T) {
	type identity struct{ kind, namespace, name string }
	index := map[identity]*unstructured.Unstructured{}
	examples := exampleResources(t)
	for _, example := range examples {
		object := example.object
		index[identity{object.GetKind(), object.GetNamespace(), object.GetName()}] = object
	}
	for _, example := range examples {
		object := example.object
		if object.GetKind() != "AgentDefinition" && object.GetKind() != "AgentInstance" {
			continue
		}
		t.Run(example.path, func(t *testing.T) {
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
