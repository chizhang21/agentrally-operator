// Package connector transports native AgentRally CRs without a second domain model.
package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
)

const MaxMessageBytes = 4 << 20

// Request contains routing metadata and an opaque native Kubernetes body.
type Request struct {
	ID               string          `json:"id"`
	Method           string          `json:"method"`
	Resource         string          `json:"resource"`
	Namespace        string          `json:"namespace,omitempty"`
	Name             string          `json:"name,omitempty"`
	Body             json.RawMessage `json:"body,omitempty"`
	Limit            int64           `json:"limit,omitempty"`
	Continue         string          `json:"continue,omitempty"`
	Scope            *RequestScope   `json:"scope,omitempty"`
	ObservationScope string          `json:"observationScope,omitempty"`
}

type Response struct {
	ID     string          `json:"id"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type Executor struct {
	Client            dynamic.Interface
	Metadata          metadata.Interface
	InvocationPreview bool
}

// Execute relays allowed CR operations and exposes read-only management observations.
// The API server validates native spec; the connector never writes status.
func (e Executor) Execute(ctx context.Context, req Request) Response {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, status, err := e.execute(ctx, req)
	if err != nil {
		var apiStatus metav1.Status
		var authored *protocolFailure
		var s apierrors.APIStatus
		if errors.As(err, &authored) {
			apiStatus = authored.Status()
		} else if errors.As(err, &s) {
			apiStatus = s.Status()
			// Invalid/CEL/webhook errors can echo values from the object body.
			// Keep machine-readable classification without reflecting those values.
			apiStatus = metav1.Status{Status: metav1.StatusFailure, Code: apiStatus.Code, Reason: apiStatus.Reason,
				Message: "Kubernetes rejected the request; inspect the resource and API-server diagnostics"}
		} else {
			apiStatus = apierrors.NewInternalError(fmt.Errorf("kubernetes request failed")).Status()
		}
		apiStatus.APIVersion, apiStatus.Kind = "v1", "Status"
		body, _ = json.Marshal(apiStatus)
		status = int(apiStatus.Code)
	}
	return Response{ID: req.ID, Status: status, Body: body}
}

func (e Executor) execute(ctx context.Context, req Request) (json.RawMessage, int, error) {
	if err := validateProtocol(req); err != nil {
		return nil, 0, err
	}
	if req.Resource == "connectorcapabilities" {
		return mustJSON(managementCapabilities(e.InvocationPreview)), http.StatusOK, nil
	}
	return e.executeScoped(ctx, req)
}

func (e Executor) executeNative(ctx context.Context, req Request) (json.RawMessage, int, error) {
	kind, err := validateRequest(req)
	if err != nil {
		return nil, 0, err
	}
	resource := e.Client.Resource(resourceAPI(req.Resource))
	var client dynamic.ResourceInterface = resource
	if req.Namespace != "" {
		client = resource.Namespace(req.Namespace)
	}
	var result any
	status := http.StatusOK
	switch req.Method {
	case http.MethodGet:
		if len(req.Body) != 0 {
			return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "GET must not specify a body")
		}
		if req.Name == "" {
			result, err = client.List(ctx, listOptions(req))
		} else {
			result, err = client.Get(ctx, req.Name, metav1.GetOptions{})
		}
	case http.MethodPost, http.MethodPut:
		object, decodeErr := requestObject(req, kind)
		if decodeErr != nil {
			return nil, 0, decodeErr
		}
		if req.Method == http.MethodPost {
			result, err = client.Create(ctx, object, metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict})
			status = http.StatusCreated
		} else {
			result, err = client.Update(ctx, object, metav1.UpdateOptions{FieldValidation: metav1.FieldValidationStrict})
		}
	case http.MethodDelete:
		options, decodeErr := requestDeleteOptions(req)
		if decodeErr != nil {
			return nil, 0, decodeErr
		}
		err = client.Delete(ctx, req.Name, options)
		status = http.StatusAccepted
		result = metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusSuccess, Code: int32(status)}
	default:
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "unsupported method")
	}
	if err != nil {
		return nil, 0, err
	}
	data, err := json.Marshal(result)
	return data, status, err
}

// validateRequest constrains routing without redefining the native CR spec.
func validateRequest(req Request) (string, error) {
	kind := ""
	switch req.Resource {
	case "agentgroups":
		kind = "AgentGroup"
	case "agentinstances":
		kind = "AgentInstance"
	case "agentdefinitions":
		kind = "AgentDefinition"
	case "persistentvolumeclaims":
		kind = "PersistentVolumeClaim"
		if req.Method != http.MethodGet {
			return "", protocolError(http.StatusForbidden, "Forbidden", "PVC references are read only")
		}
	case "secrets":
		kind = "Secret"
		if req.Method != http.MethodGet && req.Method != http.MethodPost && req.Method != http.MethodDelete {
			return "", protocolError(http.StatusForbidden, "Forbidden", "Secrets only support creation, guarded deletion and metadata reads")
		}
	case "configmaps":
		kind = "ConfigMap"
		if req.Method != http.MethodGet && req.Method != http.MethodPost && req.Method != http.MethodDelete {
			return "", protocolError(http.StatusForbidden, "Forbidden", "ConfigMaps only support creation, guarded deletion and metadata reads")
		}
	case "clustercapabilities":
		kind = "ClusterCapabilities"
		if req.Method != http.MethodGet {
			return "", apierrors.NewForbidden(schema.GroupResource{Group: "arally.io", Resource: req.Resource}, req.Name, fmt.Errorf("capabilities are read only"))
		}
	default:
		return "", protocolError(http.StatusBadRequest, "BadRequest", "unsupported resource")
	}
	if req.Resource == "agentinstances" || req.Resource == "agentdefinitions" || req.Resource == "secrets" || req.Resource == "configmaps" || req.Resource == "persistentvolumeclaims" {
		if req.Namespace == "" || len(validation.IsDNS1123Label(req.Namespace)) != 0 {
			return "", protocolError(http.StatusBadRequest, "BadRequest", "namespaced resources require a valid namespace")
		}
	} else if req.Namespace != "" {
		return "", protocolError(http.StatusBadRequest, "BadRequest", "cluster scoped resources cannot specify namespace")
	}
	if req.Name != "" && len(validation.IsDNS1123Subdomain(req.Name)) != 0 {
		return "", protocolError(http.StatusBadRequest, "BadRequest", "invalid resource name")
	}
	if req.Limit < 0 || req.Limit > 500 {
		return "", protocolError(http.StatusBadRequest, "BadRequest", "list limit must be between 0 and 500")
	}
	if req.Method != http.MethodGet && (req.Limit != 0 || req.Continue != "") {
		return "", protocolError(http.StatusBadRequest, "BadRequest", "pagination is only supported for GET lists")
	}
	if req.Name != "" && (req.Limit != 0 || req.Continue != "") {
		return "", protocolError(http.StatusBadRequest, "BadRequest", "named operations cannot specify pagination")
	}
	return kind, nil
}

func requestObject(req Request, kind string) (*unstructured.Unstructured, error) {
	var object unstructured.Unstructured
	if len(req.Body) > MaxMessageBytes || json.Unmarshal(req.Body, &object) != nil || object.Object == nil {
		return nil, protocolError(http.StatusBadRequest, "BadRequest", "body must be a native Kubernetes object")
	}
	if object.GetAPIVersion() != "arally.io/v1alpha1" || object.GetKind() != kind {
		return nil, protocolError(http.StatusBadRequest, "BadRequest", "apiVersion or kind does not match resource")
	}
	if object.GetName() == "" || len(validation.IsDNS1123Subdomain(object.GetName())) != 0 || object.GetGenerateName() != "" {
		return nil, protocolError(http.StatusBadRequest, "BadRequest", "explicit metadata.name is required; generateName is unsupported")
	}
	if object.GetNamespace() != req.Namespace {
		return nil, protocolError(http.StatusBadRequest, "BadRequest", "metadata.namespace does not match request namespace")
	}
	delete(object.Object, "status")
	if req.Method == http.MethodPost {
		if req.Name != "" || object.GetResourceVersion() != "" || object.GetUID() != "" {
			return nil, protocolError(http.StatusBadRequest, "BadRequest", "create uses the collection endpoint without uid or resourceVersion")
		}
		metadata, _, err := unstructured.NestedMap(object.Object, "metadata")
		if err != nil {
			return nil, protocolError(http.StatusBadRequest, "BadRequest", "invalid creation metadata")
		}
		for field := range metadata {
			switch field {
			case "name", "namespace", "labels", "annotations":
			default:
				return nil, protocolError(http.StatusBadRequest, "BadRequest", "create metadata only supports name, namespace, labels and annotations")
			}
		}
		if err := markCreation(&object, req.ID); err != nil {
			return nil, err
		}
	} else {
		if req.Name == "" || req.Name != object.GetName() || object.GetResourceVersion() == "" {
			return nil, protocolError(http.StatusBadRequest, "BadRequest", "update requires matching name and metadata.resourceVersion")
		}
	}
	return &object, nil
}

func resourceAPI(name string) schema.GroupVersionResource {
	if name == "secrets" || name == "configmaps" || name == "persistentvolumeclaims" {
		return schema.GroupVersionResource{Version: "v1", Resource: name}
	}
	return nativeResource(name)
}

func listOptions(req Request) metav1.ListOptions {
	limit := req.Limit
	if limit == 0 {
		limit = 100
	}
	return metav1.ListOptions{Limit: limit, Continue: req.Continue}
}

const operationAnnotation = "arally.io/connector-operation-id"

func markCreation(object *unstructured.Unstructured, id string) error {
	if !wireID.MatchString(id) {
		return protocolError(http.StatusBadRequest, "BadRequest", "create requires a valid operation id")
	}
	annotations := object.GetAnnotations()
	if _, reserved := annotations[operationAnnotation]; reserved {
		return protocolError(http.StatusBadRequest, "BadRequest", "creation operation annotation is managed by Connector")
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[operationAnnotation] = id
	object.SetAnnotations(annotations)
	return nil
}

func requestDeleteOptions(req Request) (metav1.DeleteOptions, error) {
	var options metav1.DeleteOptions
	decoder := json.NewDecoder(bytes.NewReader(req.Body))
	decoder.DisallowUnknownFields()
	if req.Name == "" || len(req.Body) > MaxMessageBytes || decoder.Decode(&options) != nil || decoder.Decode(new(any)) != io.EOF ||
		options.APIVersion != "" && options.APIVersion != "v1" || options.Kind != "" && options.Kind != "DeleteOptions" || options.Preconditions == nil ||
		options.Preconditions.UID == nil || *options.Preconditions.UID == "" ||
		options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion == "" {
		return metav1.DeleteOptions{}, protocolError(http.StatusBadRequest, "BadRequest", "delete requires name and strict DeleteOptions with UID/resourceVersion preconditions")
	}
	//nolint:staticcheck // Reject deprecated orphaning requests rather than silently accepting them.
	if options.GracePeriodSeconds != nil || options.OrphanDependents != nil || len(options.DryRun) != 0 || options.IgnoreStoreReadErrorWithClusterBreakingPotential != nil {
		return metav1.DeleteOptions{}, protocolError(http.StatusBadRequest, "BadRequest", "delete supports only concurrency preconditions and foreground propagation")
	}
	if options.PropagationPolicy != nil && *options.PropagationPolicy != metav1.DeletePropagationForeground {
		return metav1.DeleteOptions{}, protocolError(http.StatusBadRequest, "BadRequest", "delete requires foreground propagation")
	}
	foreground := metav1.DeletePropagationForeground
	options.PropagationPolicy = &foreground
	return options, nil
}
