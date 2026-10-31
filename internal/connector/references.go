package connector

import (
	"context"
	"encoding/json"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// createReference shares the write boundary of the two explicitly allowed DTOs.
func (e Executor) createReference(ctx context.Context, req Request, input runtime.Object) (json.RawMessage, int, error) {
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(input)
	if err != nil {
		return nil, 0, err
	}
	object := &unstructured.Unstructured{Object: fields}
	if err := markCreation(object, req.ID); err != nil {
		return nil, 0, err
	}
	created, err := e.Client.Resource(resourceAPI(req.Resource)).Namespace(req.Namespace).Create(ctx, object, metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict})
	if err != nil {
		return nil, 0, err
	}
	return mustJSON(metadataObject(created)), http.StatusCreated, nil
}

func (e Executor) deleteCreatedReference(ctx context.Context, req Request) (json.RawMessage, int, error) {
	options, err := requestDeleteOptions(req)
	if err != nil {
		return nil, 0, err
	}
	resource, err := e.metadataResource(req)
	if err != nil {
		return nil, 0, err
	}
	current, err := resource.Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, 0, err
	}
	if current.Name != req.Name || current.Namespace != req.Namespace || current.UID == "" || current.ResourceVersion == "" {
		return nil, 0, unexpectedMetadata()
	}
	// The marker guards against accidental deletion, not a forged caller.
	// The management server must also authorize against its creation record.
	if !wireID.MatchString(current.Annotations[operationAnnotation]) {
		return nil, 0, protocolError(http.StatusForbidden, "Forbidden", "Resource deletion requires a valid Connector creation marker")
	}
	if *options.Preconditions.UID != current.UID || *options.Preconditions.ResourceVersion != current.ResourceVersion {
		return nil, 0, protocolError(http.StatusConflict, "Conflict", "Resource UID or resourceVersion precondition is stale")
	}
	// Keep both native preconditions: the target can change after this read.
	return e.executeNative(ctx, req)
}
