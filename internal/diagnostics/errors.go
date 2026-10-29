// Package diagnostics separates safe user diagnostics from their original causes.
package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
)

// Error carries a safe user message separately from its cause.
// Reader errors remain available to errors.Is/As without entering Conditions.
type Error struct {
	reason, message string
	cause           error
}

func (e *Error) Error() string { return e.message }

func (e *Error) Unwrap() error { return e.cause }

// New separates a safe public diagnostic from its underlying cause.
func New(reason, message string, cause error) error {
	return &Error{reason: reason, message: message, cause: cause}
}

// IsPending identifies configuration and missing-dependency waits. Known API and
// transport failures require normal error backoff, even inside safe diagnostics.
func IsPending(err error) bool {
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return true
	}
	var status apierrors.APIStatus
	var networkError net.Error
	if errors.As(err, &status) || errors.As(err, &networkError) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var diagnostic *Error
	return errors.As(err, &diagnostic)
}

// Classify identifies dependency failures without exposing raw error text.
func Classify(err error) (reason, message string) {
	if err == nil {
		return "", ""
	}
	var diagnostic *Error
	if errors.As(err, &diagnostic) {
		return diagnostic.reason, diagnostic.message
	}
	if apierrors.IsNotFound(err) {
		return "DependencyNotFound", "a required Kubernetes resource is missing; create the resource or correct its reference"
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return "DependencyAccessDenied", "cannot read a required Kubernetes resource; check the operator's service account permissions"
	}
	var networkError net.Error
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) || apierrors.IsInternalError(err) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.As(err, &networkError) {
		return "DependencyReadFailed", "cannot read a required Kubernetes resource; check API server connectivity and retry"
	}
	return "InvalidConfiguration", "the AgentInstance configuration is invalid; check its fields and resource references"
}

// SafeError is the public error boundary for status, Events and logs.
// Keep safe project diagnostics intact and retain raw causes only for errors.Is/As.
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	var diagnostic *Error
	if errors.As(err, &diagnostic) {
		return New(diagnostic.reason, diagnostic.message, err)
	}
	reason, message := Classify(err)
	if reason == "InvalidConfiguration" {
		message = "Cannot reconcile Kubernetes resources; check resource ownership, operator permissions and API server connectivity"
	}
	return New("ReconcileError", message, err)
}

// ReferenceError identifies the referenced resource while retaining a safe message.
func ReferenceError(kind, namespace, name string, err error) error {
	reason, message := Classify(err)
	return New(reason, fmt.Sprintf("%s %s/%s: %s", kind, namespace, name, message), err)
}
