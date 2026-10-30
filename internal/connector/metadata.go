package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
)

// NewMetadataClient disables client-go's full-object content negotiation fallback.
// The API server must support PartialObjectMetadata; a 406 is never retried as a
// full Secret read. Metadata RBAC still uses the native get/list verbs.
func NewMetadataClient(config *rest.Config) (metadata.Interface, error) {
	config = rest.CopyConfig(config)
	config.Wrap(func(base http.RoundTripper) http.RoundTripper { return metadataTransport{base} })
	return metadata.NewForConfig(config)
}

type metadataTransport struct{ http.RoundTripper }

func (transport metadataTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	kind := "PartialObjectMetadata"
	if strings.Contains(request.Header.Get("Accept"), "as=PartialObjectMetadataList;") {
		kind += "List"
	}
	request.Header.Set("Accept", "application/json;as="+kind+";g=meta.k8s.io;v=v1")
	response, err := transport.RoundTripper.RoundTrip(request)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, err
	}
	// client-go clears TypeMeta on decoded objects, including its full-object
	// fallback. Validate the bounded wire representation before that decoder.
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxMessageBytes+1))
	_ = response.Body.Close()
	if err != nil || len(data) > MaxMessageBytes {
		return nil, unexpectedMetadata()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var object any
	var gvk *metav1.TypeMeta
	if kind == "PartialObjectMetadataList" {
		list := new(metav1.PartialObjectMetadataList)
		object, gvk = list, &list.TypeMeta
	} else {
		partial := new(metav1.PartialObjectMetadata)
		object, gvk = partial, &partial.TypeMeta
	}
	if decoder.Decode(object) != nil || decoder.Decode(new(any)) != io.EOF || gvk.APIVersion != "meta.k8s.io/v1" || gvk.Kind != kind {
		return nil, unexpectedMetadata()
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

func identityMetadata(object metav1.Object) metav1.ObjectMeta {
	identity := metav1.ObjectMeta{Name: object.GetName(), Namespace: object.GetNamespace(), UID: object.GetUID(),
		ResourceVersion: object.GetResourceVersion(), CreationTimestamp: object.GetCreationTimestamp(), DeletionTimestamp: object.GetDeletionTimestamp()}
	if id := object.GetAnnotations()[operationAnnotation]; wireID.MatchString(id) {
		identity.Annotations = map[string]string{operationAnnotation: id}
	}
	return identity
}

func metadataObject(object metav1.Object) metav1.PartialObjectMetadata {
	return metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadata"}, ObjectMeta: identityMetadata(object)}
}

func (e Executor) metadataRead(ctx context.Context, req Request) (json.RawMessage, int, error) {
	if len(req.Body) != 0 {
		return nil, 0, protocolError(http.StatusBadRequest, "BadRequest", "GET must not specify a body")
	}
	if e.Metadata == nil {
		return nil, 0, unsupported("Metadata discovery is not configured")
	}
	var resource metadata.ResourceInterface = e.Metadata.Resource(resourceAPI(req.Resource))
	if req.Namespace != "" {
		resource = e.Metadata.Resource(resourceAPI(req.Resource)).Namespace(req.Namespace)
	}
	if req.Name != "" {
		object, err := resource.Get(ctx, req.Name, metav1.GetOptions{})
		if err != nil {
			return nil, 0, err
		}
		return mustJSON(metadataObject(object)), http.StatusOK, nil
	}
	list, err := resource.List(ctx, listOptions(req))
	if err != nil {
		return nil, 0, err
	}
	result := metav1.PartialObjectMetadataList{TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadataList"},
		ListMeta: metav1.ListMeta{ResourceVersion: list.ResourceVersion, Continue: list.Continue}, Items: make([]metav1.PartialObjectMetadata, 0, len(list.Items))}
	for i := range list.Items {
		result.Items = append(result.Items, metadataObject(&list.Items[i]))
	}
	return mustJSON(result), http.StatusOK, nil
}

func unexpectedMetadata() error {
	return protocolError(http.StatusBadGateway, "UnexpectedMetadataResponse", "Kubernetes did not return the requested metadata representation")
}
