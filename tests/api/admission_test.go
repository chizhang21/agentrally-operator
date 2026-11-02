package api_test

import (
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

type admissionSchema struct {
	structural *structuralschema.Structural
	validate   func(value, old map[string]interface{}) field.ErrorList
}

func newAdmissionSchema(t *testing.T, public *apiextensionsv1.JSONSchemaProps, root bool) admissionSchema {
	t.Helper()
	schema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(public, schema, nil); err != nil {
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
	cel := structuralcel.NewValidator(structural, root, 1_000_000)
	return admissionSchema{structural: structural, validate: func(value, old map[string]interface{}) field.ErrorList {
		errors := apiservervalidation.ValidateCustomResource(field.NewPath("resource"), value, validator)
		if cel != nil {
			celErrors, _ := cel.Validate(t.Context(), field.NewPath("resource"), structural, value, old, 1_000_000)
			errors = append(errors, celErrors...)
		}
		return errors
	}}
}
