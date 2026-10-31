package connector

import (
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RequestScope constrains resource identity. The authenticated management server
// must authorize the caller; possession of these identifiers is not a credential.
type RequestScope struct {
	GroupName    string `json:"groupName"`
	GroupUID     string `json:"groupUID,omitempty"`
	NamespaceUID string `json:"namespaceUID,omitempty"`
}

type capabilityDocument struct {
	Resources             map[string][]string `json:"resources"`
	MaxMessageBytes       int                 `json:"maxMessageBytes"`
	MaxListLimit          int                 `json:"maxListLimit"`
	InvocationPreview     bool                `json:"invocationPreview"`
	ExclusiveHandoff      bool                `json:"exclusiveHandoff"`
	Invocation            bool                `json:"invocation"`
	ProcessIdentity       bool                `json:"processIdentity"`
	ScopedNamespaceCreate bool                `json:"scopedNamespaceCreate"`
	SecretCreateTypes     []corev1.SecretType `json:"secretCreateTypes"`
	InstanceDiagnostics   bool                `json:"instanceDiagnostics"`
}

func managementCapabilities(preview bool) capabilityDocument {
	crud := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete}
	get := []string{http.MethodGet}
	return capabilityDocument{
		InvocationPreview:     preview,
		ScopedNamespaceCreate: true,
		SecretCreateTypes:     secretCreateTypes,
		InstanceDiagnostics:   true,
		MaxMessageBytes:       MaxMessageBytes, MaxListLimit: 500,
		Resources: map[string][]string{
			"agentgroups": crud, "agentdefinitions": crud, "agentinstances": crud, "clustercapabilities": get,
			"namespaces": get, "instanceobservations": get,
			"persistentvolumeclaims": get, "secrets": {http.MethodGet, http.MethodPost, http.MethodDelete},
			"configmaps": {http.MethodGet, http.MethodPost, http.MethodDelete},
		},
	}
}

// protocolFailure carries only connector-authored messages. Native API errors
// may include submitted values and must not use this trusted-message path.
type protocolFailure struct{ *apierrors.StatusError }

func protocolError(code int, reason, message string) error {
	return &protocolFailure{&apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: int32(code), Reason: metav1.StatusReason(reason), Message: message,
	}}}
}

func unsupported(message string) error {
	return protocolError(http.StatusNotImplemented, "Unsupported", message)
}

func validateProtocol(req Request) error {
	if len(req.Continue) > 16*1024 {
		return protocolError(http.StatusBadRequest, "BadRequest", "continue token exceeds connector limit")
	}
	if req.ObservationScope != "" {
		if req.Resource != "instanceobservations" || req.Method != http.MethodGet {
			return protocolError(http.StatusBadRequest, "BadRequest", "observationScope is only valid for instance observations")
		}
		switch req.ObservationScope {
		case "Container":
		case "Process":
			return unsupported("Process identity cannot be verified from container observations")
		default:
			return protocolError(http.StatusBadRequest, "BadRequest", "unsupported observation scope")
		}
	}
	if req.Resource == "exclusivehandoff" {
		return unsupported("Exclusive handoff requires verified runtime or external fencing; no provider is available")
	}
	if req.Resource == "connectorcapabilities" {
		if req.Method != http.MethodGet || req.Namespace != "" || req.Name != "" || len(req.Body) != 0 || req.Limit != 0 || req.Continue != "" || req.Scope != nil {
			return protocolError(http.StatusBadRequest, "BadRequest", "connector capability discovery accepts only an unscoped GET")
		}
	}
	return nil
}
