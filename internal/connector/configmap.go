package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (e Executor) createConfigMap(ctx context.Context, req Request) (json.RawMessage, int, error) {
	var input struct {
		metav1.TypeMeta `json:",inline"`
		Metadata        struct{ Name, Namespace string } `json:"metadata"`
		Data            map[string]string                `json:"data,omitempty"`
		BinaryData      map[string][]byte                `json:"binaryData,omitempty"`
		Immutable       *bool                            `json:"immutable,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Body))
	decoder.DisallowUnknownFields()
	if len(req.Body) > MaxMessageBytes || decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF ||
		input.APIVersion != "v1" || input.Kind != "ConfigMap" || req.Name != "" ||
		len(validation.IsDNS1123Subdomain(input.Metadata.Name)) != 0 || input.Metadata.Namespace != req.Namespace {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "create requires an explicitly named ConfigMap with only supported metadata fields")
	}
	// Empty ConfigMaps are valid; key overlap and size are native admission rules.
	object := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: input.Metadata.Name, Namespace: input.Metadata.Namespace},
		Data:       input.Data, BinaryData: input.BinaryData, Immutable: input.Immutable}
	return e.createReference(ctx, req, object)
}
