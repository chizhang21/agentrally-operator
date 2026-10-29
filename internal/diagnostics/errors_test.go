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
		err     error
		reason  string
		pending bool
	}{
		{nil, "", false},
		{fmt.Errorf("wrapped: %w", New("CustomReason", "safe action", nil)), "CustomReason", true},
		{New("CustomReason", "safe action", fmt.Errorf("wrapped: %w", New("OwnershipConflict", "safe ownership", nil))), "CustomReason", true},
		{New("InvalidConfiguration", "safe configuration", errors.New("invalid format")), "InvalidConfiguration", true},
		{fmt.Errorf("wrapped: %w", New("CustomReason", "safe action", context.DeadlineExceeded)), "CustomReason", false},
		{fmt.Errorf("wrapped: %w", apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "key")), "DependencyNotFound", true},
		{apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "key", errors.New("private-detail")), "DependencyAccessDenied", false},
		{New("CannotRead", "safe action", apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "key", errors.New("private-detail"))), "CannotRead", false},
		{apierrors.NewUnauthorized("private-detail"), "DependencyAccessDenied", false},
		{apierrors.NewTimeoutError("private-detail", 1), "DependencyReadFailed", false},
		{context.DeadlineExceeded, "DependencyReadFailed", false},
		{errors.New("private-detail"), "InvalidConfiguration", false},
	} {
		reason, message := Classify(tc.err)
		if reason != tc.reason || strings.Contains(message, "private-detail") {
			t.Fatalf("reason=%s message=%s", reason, message)
		}
		if IsPending(tc.err) != tc.pending {
			t.Fatalf("incorrect wait/retry classification for %v", tc.err)
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
