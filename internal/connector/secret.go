package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (e Executor) createSecret(ctx context.Context, req Request) (json.RawMessage, int, error) {
	// A narrow DTO excludes arbitrary annotations, service-account references,
	// ownership, finalizers and server-assigned identity before a write is sent.
	var input struct {
		metav1.TypeMeta `json:",inline"`
		Metadata        struct{ Name, Namespace string } `json:"metadata"`
		Type            corev1.SecretType                `json:"type,omitempty"`
		Data            map[string][]byte                `json:"data,omitempty"`
		StringData      map[string]string                `json:"stringData,omitempty"`
		Immutable       *bool                            `json:"immutable,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Body))
	decoder.DisallowUnknownFields()
	if len(req.Body) > MaxMessageBytes || decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF ||
		input.APIVersion != "v1" || input.Kind != "Secret" || req.Name != "" ||
		len(validation.IsDNS1123Subdomain(input.Metadata.Name)) != 0 || input.Metadata.Namespace != req.Namespace ||
		input.Type != "" && input.Type != corev1.SecretTypeOpaque || len(input.Data)+len(input.StringData) == 0 {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "create requires an explicitly named Opaque Secret with data and only supported metadata fields")
	}
	secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: input.Metadata.Name, Namespace: input.Metadata.Namespace},
		Type:       corev1.SecretTypeOpaque, Data: input.Data, StringData: input.StringData, Immutable: input.Immutable}
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(secret)
	if err != nil {
		return nil, 0, err
	}
	object := &unstructured.Unstructured{Object: fields}
	if err := markCreation(object, req.ID); err != nil {
		return nil, 0, err
	}
	created, err := e.Client.Resource(resourceAPI("secrets")).Namespace(req.Namespace).Create(ctx, object, metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict})
	if err != nil {
		return nil, 0, err
	}
	return mustJSON(metadataObject(created)), http.StatusCreated, nil
}
