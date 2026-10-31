package connector

import (
	"context"
	"encoding/json"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func (e Executor) pvcRead(ctx context.Context, req Request) (json.RawMessage, int, error) {
	if len(req.Body) != 0 {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "GET must not specify a body")
	}
	resource := e.Client.Resource(resourceAPI(req.Resource)).Namespace(req.Namespace)
	if req.Name != "" {
		object, err := resource.Get(ctx, req.Name, metav1.GetOptions{})
		if err != nil {
			return nil, 0, err
		}
		projected, err := pvcObject(object)
		return mustJSON(projected), http.StatusOK, err
	}
	list, err := resource.List(ctx, listOptions(req))
	if err != nil {
		return nil, 0, err
	}
	result := &unstructured.UnstructuredList{Items: make([]unstructured.Unstructured, 0, len(list.Items))}
	result.SetAPIVersion(list.GetAPIVersion())
	result.SetKind(list.GetKind())
	result.SetResourceVersion(list.GetResourceVersion())
	result.SetContinue(list.GetContinue())
	for i := range list.Items {
		object := &list.Items[i]
		projected, err := pvcObject(object)
		if err != nil {
			return nil, 0, err
		}
		result.Items = append(result.Items, *projected)
	}
	return mustJSON(result), http.StatusOK, nil
}

func pvcObject(object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	identity := identityMetadata(object)
	metadata, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&identity)
	if err != nil {
		return nil, err
	}
	result := &unstructured.Unstructured{Object: map[string]any{"apiVersion": object.GetAPIVersion(), "kind": object.GetKind(), "metadata": metadata}}
	fields := [][]string{{"spec", "accessModes"}, {"spec", "volumeMode"}, {"spec", "storageClassName"}, {"spec", "resources", "requests", "storage"}, {"status", "phase"}, {"status", "capacity", "storage"}}
	for _, path := range fields {
		value, found, err := unstructured.NestedFieldCopy(object.Object, path...)
		if err != nil {
			return nil, err
		}
		if found {
			if err := unstructured.SetNestedField(result.Object, value, path...); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
