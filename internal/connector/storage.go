package connector

import (
	"context"
	"encoding/json"
	"net/http"

	"agentrally/operator/internal/workspace"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

func (e Executor) storageRead(ctx context.Context, req Request, namespace *unstructured.Unstructured) (json.RawMessage, int, error) {
	if len(req.Body) != 0 {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "GET must not specify a body")
	}
	resource := e.Client.Resource(resourceAPI(req.Resource))
	if req.Name != "" {
		var object *unstructured.Unstructured
		var err error
		if req.Namespace != "" {
			object, err = resource.Namespace(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
		} else {
			object, err = resource.Get(ctx, req.Name, metav1.GetOptions{})
		}
		if err != nil {
			return nil, 0, err
		}
		if req.Resource == "agentworkspaces" && !workspaceMatches(object, req.Scope, namespace.GetName()) {
			return nil, 0, scopeForbidden()
		}
		projected, err := storageObject(object, req.Resource)
		return mustJSON(projected), http.StatusOK, err
	}
	options := listOptions(req)
	var list *unstructured.UnstructuredList
	var err error
	if req.Resource == "agentworkspaces" {
		options.LabelSelector = labels.Set{workspace.GroupUIDLabel: req.Scope.GroupUID, workspace.NamespaceUIDLabel: req.Scope.NamespaceUID}.String()
		list, err = resource.List(ctx, options)
	} else {
		list, err = resource.Namespace(req.Namespace).List(ctx, options)
	}
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
		if req.Resource == "agentworkspaces" && !workspaceMatches(object, req.Scope, namespace.GetName()) {
			continue
		}
		projected, err := storageObject(object, req.Resource)
		if err != nil {
			return nil, 0, err
		}
		result.Items = append(result.Items, *projected)
	}
	return mustJSON(result), http.StatusOK, nil
}

func workspaceMatches(object *unstructured.Unstructured, scope *RequestScope, namespace string) bool {
	for field, want := range map[string]string{"groupName": scope.GroupName, "groupUID": scope.GroupUID, "namespace": namespace, "namespaceUID": scope.NamespaceUID} {
		value, _, err := unstructured.NestedString(object.Object, "spec", field)
		if err != nil || value != want {
			return false
		}
	}
	return true
}

func storageObject(object *unstructured.Unstructured, resource string) (*unstructured.Unstructured, error) {
	identity := identityMetadata(object)
	metadata, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&identity)
	if err != nil {
		return nil, err
	}
	result := &unstructured.Unstructured{Object: map[string]any{"apiVersion": object.GetAPIVersion(), "kind": object.GetKind(), "metadata": metadata}}
	fields := [][]string{{"spec"}, {"status"}}
	if resource == "persistentvolumeclaims" {
		fields = [][]string{{"spec", "accessModes"}, {"spec", "volumeMode"}, {"spec", "storageClassName"}, {"spec", "resources", "requests", "storage"}, {"status", "phase"}, {"status", "capacity", "storage"}}
	}
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
