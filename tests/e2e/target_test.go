//go:build e2e

package e2e_test

import "testing"

func TestE2ETargetGuard(t *testing.T) {
	for _, test := range []struct {
		name, context, server, expectedContext, expectedServer string
		allowed                                                bool
	}{
		{"disposable kind", "kind-arally-e2e-test", "https://127.0.0.1:6443", "", "", true},
		{"shared target rejected by default", "vanilla", "https://192.0.2.1:6443", "", "", false},
		{"explicit Kata target", "vanilla", "https://192.0.2.1:6443", "vanilla", "https://192.0.2.1:6443", true},
		{"context alone", "vanilla", "https://192.0.2.1:6443", "vanilla", "", false},
		{"server alone", "vanilla", "https://192.0.2.1:6443", "", "https://192.0.2.1:6443", false},
		{"wrong context", "other", "https://192.0.2.1:6443", "vanilla", "https://192.0.2.1:6443", false},
		{"wrong server", "vanilla", "https://192.0.2.2:6443", "vanilla", "https://192.0.2.1:6443", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateE2ETarget(test.context, test.server, test.expectedContext, test.expectedServer)
			if (err == nil) != test.allowed {
				t.Fatalf("unexpected target acceptance: %v", err)
			}
		})
	}
}
