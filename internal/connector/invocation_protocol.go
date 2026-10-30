package connector

import (
	"bytes"
	"encoding/json"
	"io"
	"k8s.io/apimachinery/pkg/util/validation"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	InvocationProtocol    = "invocation"
	MaxInvocationBody     = 1 << 20
	MaxInvocationResponse = 4 << 20
	MaxInvocationEnvelope = 6 << 20
	MaxStreamChunk        = 32 << 10
	MaxSSEEvent           = 1 << 20
)

var wireID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type InvocationIdentity struct {
	Scope       RequestScope `json:"scope"`
	InstanceID  string       `json:"instanceId"`
	InstanceUID string       `json:"instanceUID"`
	ExecutionID string       `json:"executionId"`
}

type InvocationCommand struct {
	InvocationIdentity
	ConnectionEpoch string            `json:"connectionEpoch"`
	RequestID       string            `json:"requestId"`
	GateEpoch       uint64            `json:"gateEpoch"`
	Operation       string            `json:"operation"`
	IssuedAt        time.Time         `json:"issuedAt"`
	ExpiresAt       time.Time         `json:"expiresAt"`
	ContentType     string            `json:"contentType,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            json.RawMessage   `json:"body,omitempty"`
}

type InvocationResult struct {
	RequestID   string            `json:"requestId"`
	ExecutionID string            `json:"executionId"`
	Delivery    string            `json:"delivery"`
	StatusCode  int               `json:"statusCode,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        []byte            `json:"bodyBase64,omitempty"`
	Reason      string            `json:"reason"`
}

type InvocationFrame struct {
	RequestID   string            `json:"requestId"`
	ExecutionID string            `json:"executionId"`
	Sequence    uint64            `json:"sequence"`
	Type        string            `json:"type"`
	StatusCode  int               `json:"statusCode,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Data        []byte            `json:"dataBase64,omitempty"`
	Reason      string            `json:"reason,omitempty"`
}

type InvocationControl struct {
	InvocationIdentity
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	ConnectionEpoch string    `json:"connectionEpoch"`
	GateEpoch       uint64    `json:"gateEpoch,omitempty"`
	Action          string    `json:"action,omitempty"`
	RequestID       string    `json:"requestId,omitempty"`
	IssuedAt        time.Time `json:"issuedAt,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}

type InvocationControlResult struct {
	ID            string     `json:"id"`
	Status        int        `json:"status"`
	Reason        string     `json:"reason"`
	GateEpoch     uint64     `json:"gateEpoch,omitempty"`
	ActiveStarts  int        `json:"activeStarts"`
	DrainDeadline *time.Time `json:"drainDeadline,omitempty"`
}

// Errors on this path use fixed reasons only, never application or server text.
type invocationError string

func (e invocationError) Error() string { return string(e) }

func validateInvocationIdentity(identity InvocationIdentity) error {
	if err := validateScope(&identity.Scope, true, true); err != nil {
		return invocationError("InvalidScope")
	}
	if len(validation.IsDNS1123Subdomain(identity.InstanceID)) != 0 || !wireID.MatchString(identity.InstanceUID) || !wireID.MatchString(identity.ExecutionID) {
		return invocationError("InvalidIdentity")
	}
	return nil
}

func validateInvocationWindow(issued, expires time.Time, now time.Time, maximum time.Duration) error {
	_, issuedOffset := issued.Zone()
	_, expiresOffset := expires.Zone()
	if issued.IsZero() || expires.IsZero() || issuedOffset != 0 || expiresOffset != 0 || issued.After(now.Add(5*time.Second)) || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > maximum {
		return invocationError("InvalidOrExpiredWindow")
	}
	return nil
}

func validateInvocation(command InvocationCommand, epoch string, now time.Time) error {
	if command.ConnectionEpoch != epoch || !wireID.MatchString(command.RequestID) {
		return invocationError("InvalidSessionOrRequest")
	}
	if err := validateInvocationIdentity(command.InvocationIdentity); err != nil {
		return err
	}
	maximum := 300 * time.Second
	switch command.Operation {
	case "Ping":
		maximum = 5 * time.Second
		if len(command.Body) != 0 || command.GateEpoch != 0 {
			return invocationError("InvalidPing")
		}
	case "Start":
		body := bytes.TrimSpace(command.Body)
		if command.GateEpoch == 0 || command.ContentType != "application/json" || len(body) == 0 || len(body) > MaxInvocationBody || !utf8.Valid(body) || !json.Valid(body) || body[0] != '{' && body[0] != '"' {
			return invocationError("InvalidStartBody")
		}
	default:
		return invocationError("UnsupportedOperation")
	}
	if err := validateInvocationWindow(command.IssuedAt, command.ExpiresAt, now, maximum); err != nil {
		return err
	}
	seen := map[string]bool{}
	for name, value := range command.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if (canonical != "Accept" && canonical != "Idempotency-Key" && canonical != "Traceparent") || seen[canonical] || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
			return invocationError("InvalidHeader")
		}
		seen[canonical] = true
	}
	return nil
}

func invocationReason(err error) string {
	if reason, ok := err.(invocationError); ok {
		return string(reason)
	}
	return "TargetObservationUnavailable"
}

func decodeInvocationJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invocationError("InvalidEnvelope")
	}
	if decoder.More() {
		return invocationError("InvalidEnvelope")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return invocationError("InvalidEnvelope")
	}
	return nil
}
