package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

var secretCreateTypes = []corev1.SecretType{corev1.SecretTypeOpaque, corev1.SecretTypeDockerConfigJson}

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
		len(input.Data)+len(input.StringData) == 0 {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "create requires an explicitly named Secret with data and only supported metadata fields")
	}
	if input.Type == "" {
		input.Type = corev1.SecretTypeOpaque
	}
	if !slices.Contains(secretCreateTypes, input.Type) {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "Secret creation only supports Opaque and kubernetes.io/dockerconfigjson")
	}
	if input.Type == corev1.SecretTypeDockerConfigJson {
		_, dataKey := input.Data[corev1.DockerConfigJsonKey]
		_, stringKey := input.StringData[corev1.DockerConfigJsonKey]
		if len(input.Data)+len(input.StringData) != 1 || !dataKey && !stringKey {
			return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "registry Secret requires only .dockerconfigjson in either data or stringData")
		}
	}
	secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: input.Metadata.Name, Namespace: input.Metadata.Namespace},
		Type:       input.Type, Data: input.Data, StringData: input.StringData, Immutable: input.Immutable}
	return e.createReference(ctx, req, secret)
}
