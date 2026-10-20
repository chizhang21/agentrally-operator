package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestConfigurationDiagnosticClassification(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{nil, ""},
		{fmt.Errorf("wrapped: %w", New("CustomReason", "safe action", context.DeadlineExceeded)), "CustomReason"},
		{fmt.Errorf("wrapped: %w", apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "key")), "DependencyNotFound"},
		{apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "key", errors.New("private-detail")), "DependencyAccessDenied"},
		{apierrors.NewUnauthorized("private-detail"), "DependencyAccessDenied"},
		{apierrors.NewTimeoutError("private-detail", 1), "DependencyReadFailed"},
		{context.DeadlineExceeded, "DependencyReadFailed"},
		{errors.New("private-detail"), "InvalidConfiguration"},
	} {
		reason, message := Classify(tc.err)
		if reason != tc.reason || strings.Contains(message, "private-detail") {
			t.Fatalf("reason=%s message=%s", reason, message)
		}
	}
}

func TestReconcileDiagnosticDoesNotExposeWrappedErrorPrefixes(t *testing.T) {
	safe := New("OwnershipConflict", "Resource belongs to another owner", nil)
	wrapped := fmt.Errorf("private-server-detail: %w", safe)
	public := SafeError(wrapped)
	if public.Error() != safe.Error() || !errors.Is(public, wrapped) || !errors.Is(public, safe) {
		t.Fatal("sanitization exposed an outer error prefix or lost the cause")
	}
}
