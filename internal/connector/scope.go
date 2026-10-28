package connector

import (
	"agentrally/operator/internal/controller"
	"context"
	"encoding/json"
	"net/http"

	v1 "agentrally/operator/api/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

func nativeResource(name string) schema.GroupVersionResource {
	return v1.GroupVersion.WithResource(name)
}

func scopeForbidden() error {
	return protocolError(http.StatusForbidden, "Forbidden", "Resource identity or ownership does not match the requested Group scope")
}

func requireOwner(child, parent *unstructured.Unstructured, apiVersion, kind string) error {
	owner := metav1.GetControllerOf(child)
	if parent.GetUID() == "" || owner == nil || owner.UID != parent.GetUID() || owner.Name != parent.GetName() || owner.APIVersion != apiVersion || owner.Kind != kind {
		return scopeForbidden()
	}
	return nil
}

func validateScope(scope *RequestScope, needUID, needNamespaceUID bool) error {
	if scope == nil || len(validation.IsDNS1123Subdomain(scope.GroupName)) != 0 ||
		len(scope.GroupUID) > 128 || len(scope.NamespaceUID) > 128 || needUID && scope.GroupUID == "" || needNamespaceUID && scope.NamespaceUID == "" {
		return protocolError(http.StatusBadRequest, "BadRequest", "a valid Group scope and the required identity preconditions must be supplied")
	}
	return nil
}

func (e Executor) scopedGroup(ctx context.Context, scope *RequestScope) (*unstructured.Unstructured, string, error) {
	group, err := e.Client.Resource(nativeResource("agentgroups")).Get(ctx, scope.GroupName, metav1.GetOptions{})
	if err != nil {
		return nil, "", err
	}
	if string(group.GetUID()) != scope.GroupUID {
		return nil, "", scopeForbidden()
	}
	var typed v1.AgentGroup
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(group.Object, &typed); err != nil {
		return nil, "", apierrors.NewInternalError(err)
	}
	return group, typed.NamespaceName(), nil
}

func (e Executor) scopedNamespace(ctx context.Context, req Request, discover bool) (*unstructured.Unstructured, error) {
	if err := validateScope(req.Scope, true, !discover); err != nil {
		return nil, err
	}
	group, name, err := e.scopedGroup(ctx, req.Scope)
	if err != nil {
		return nil, err
	}
	requested := req.Namespace
	if discover {
		requested = req.Name
	}
	if requested != name && !(discover && requested == "") {
		return nil, scopeForbidden()
	}
	namespace, err := e.Client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if namespace.GetUID() == "" || req.Scope.NamespaceUID != "" && string(namespace.GetUID()) != req.Scope.NamespaceUID {
		return nil, scopeForbidden()
	}
	var typed v1.AgentGroup
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(group.Object, &typed); err != nil {
		return nil, err
	}
	if err := controller.CheckGroupNamespace(namespace, &typed); err != nil {
		return nil, err
	}
	return namespace, nil
}

func (e Executor) executeScoped(ctx context.Context, req Request) (json.RawMessage, int, error) {
	if req.Resource == "namespaces" {
		if req.Method != http.MethodGet || req.Namespace != "" || len(req.Body) != 0 || req.Limit != 0 || req.Continue != "" {
			return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "Namespace identity discovery only supports a single read")
		}
		ns, err := e.scopedNamespace(ctx, req, true)
		if err != nil {
			return nil, 0, err
		}
		// Namespace annotations and labels are administrator-controlled arbitrary
		// data. Return an explicit whitelist, never the native Namespace body.
		identity := struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
			Group    *RequestScope     `json:"group"`
		}{Metadata: metav1.ObjectMeta{Name: ns.GetName(), UID: ns.GetUID(), ResourceVersion: ns.GetResourceVersion(), DeletionTimestamp: ns.GetDeletionTimestamp()},
			Group: &RequestScope{GroupName: req.Scope.GroupName, GroupUID: req.Scope.GroupUID, NamespaceUID: string(ns.GetUID())}}
		return mustJSON(identity), http.StatusOK, nil
	}
	if req.Resource == "instanceobservations" {
		return e.instanceObservation(ctx, req)
	}
	kind, err := validateRequest(req)
	if err != nil {
		return nil, 0, err
	}
	if req.Resource == "clustercapabilities" {
		if req.Scope != nil {
			return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "cluster discovery does not accept Group scope")
		}
		return e.executeNative(ctx, req)
	}
	if req.Method != http.MethodGet && req.Method != http.MethodPost && req.Method != http.MethodPut && req.Method != http.MethodDelete {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "unsupported method")
	}
	if req.Resource == "agentgroups" {
		return e.executeScopedGroup(ctx, req, kind)
	}
	if req.Method == http.MethodPost {
		if len(req.Body) != 0 {
			if _, err := requestObject(req, kind); err != nil {
				return nil, 0, err
			}
		}
		return nil, 0, unsupported("Scoped namespaced creation has no namespace UID precondition; this capability is unavailable")
	}
	ns, err := e.scopedNamespace(ctx, req, false)
	if err != nil {
		return nil, 0, err
	}
	if req.Method != http.MethodGet && ns.GetDeletionTimestamp() != nil {
		return nil, 0, protocolError(http.StatusConflict, "Conflict", "Namespace is terminating")
	}
	if req.Name != "" {
		object, err := e.Client.Resource(nativeResource(req.Resource)).Namespace(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
		if err != nil {
			return nil, 0, err
		}
		if err := checkGroupReference(object, req.Scope.GroupName); err != nil {
			return nil, 0, err
		}
		if err := scopedWritePreconditions(req, kind, object); err != nil {
			return nil, 0, err
		}
	}
	data, code, err := e.executeNative(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	if req.Method == http.MethodGet {
		if err := checkResponseGroup(data, req.Name == "", req.Scope.GroupName); err != nil {
			return nil, 0, err
		}
	}
	if _, err := e.scopedNamespace(ctx, req, false); err != nil {
		return nil, 0, protocolError(http.StatusConflict, "ScopeChanged", "Scope could not be verified after the operation; a write may already have taken effect")
	}
	return data, code, nil
}

func (e Executor) executeScopedGroup(ctx context.Context, req Request, kind string) (json.RawMessage, int, error) {
	if err := validateScope(req.Scope, req.Method != http.MethodPost, false); err != nil {
		return nil, 0, err
	}
	if req.Scope.NamespaceUID != "" {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "Group operations do not accept namespace identity")
	}
	if req.Method == http.MethodPost {
		body, err := requestObject(req, kind)
		if err != nil {
			return nil, 0, err
		}
		if req.Scope.GroupUID != "" || body.GetName() != req.Scope.GroupName {
			return nil, 0, scopeForbidden()
		}
	} else {
		if req.Name != req.Scope.GroupName {
			return nil, 0, scopeForbidden()
		}
		group, _, err := e.scopedGroup(ctx, req.Scope)
		if err != nil {
			return nil, 0, err
		}
		if err := scopedWritePreconditions(req, kind, group); err != nil {
			return nil, 0, err
		}
		if req.Method == http.MethodGet {
			if len(req.Body) != 0 {
				return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "GET must not specify a body")
			}
			// Return the object whose UID was authorized, without a second GET
			// that could observe a replacement under the same name.
			return mustJSON(group), http.StatusOK, nil
		}
	}
	return e.executeNative(ctx, req)
}

func scopedWritePreconditions(req Request, kind string, current *unstructured.Unstructured) error {
	switch req.Method {
	case http.MethodPut:
		body, err := requestObject(req, kind)
		if err != nil {
			return err
		}
		if body.GetUID() == "" || body.GetUID() != current.GetUID() {
			return scopeForbidden()
		}
		if req.Resource != "agentgroups" {
			return checkGroupReference(body, req.Scope.GroupName)
		}
	case http.MethodDelete:
		options, err := requestDeleteOptions(req)
		if err != nil {
			return err
		}
		if *options.Preconditions.UID != current.GetUID() {
			return scopeForbidden()
		}
	}
	return nil
}

func checkGroupReference(object *unstructured.Unstructured, name string) error {
	group, _, err := unstructured.NestedString(object.Object, "spec", "agentGroupRef", "name")
	if err != nil || group != name {
		return scopeForbidden()
	}
	return nil
}

func checkResponseGroup(data json.RawMessage, list bool, group string) error {
	if list {
		var objects unstructured.UnstructuredList
		if err := json.Unmarshal(data, &objects); err != nil {
			return apierrors.NewInternalError(err)
		}
		for i := range objects.Items {
			if err := checkGroupReference(&objects.Items[i], group); err != nil {
				return err
			}
		}
		return nil
	}
	var object unstructured.Unstructured
	if err := json.Unmarshal(data, &object); err != nil {
		return apierrors.NewInternalError(err)
	}
	return checkGroupReference(&object, group)
}
