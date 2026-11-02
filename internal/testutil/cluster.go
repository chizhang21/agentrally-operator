//go:build e2e

package testutil

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// E2EConfig loads only the driver's kubeconfig or an explicitly matched Kata target.
func E2EConfig(t *testing.T, expectedContext, expectedServer string) (*rest.Config, string) {
	t.Helper()
	path := os.Getenv("AGENTRALLY_E2E_KUBECONFIG")
	if path == "" {
		t.Fatal("run make e2e to create an isolated cluster")
	}
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateE2ETarget(raw.CurrentContext, cfg.Host, expectedContext, expectedServer); err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 30 * time.Second
	return cfg, path
}

func ValidateE2ETarget(context, server, expectedContext, expectedServer string) error {
	if expectedContext != "" || expectedServer != "" {
		if expectedContext == "" || expectedServer == "" || context != expectedContext || server != expectedServer {
			return errors.New("kata target must match both E2E_KATA_CONTEXT and E2E_KATA_API_SERVER")
		}
		return nil
	}
	if !strings.HasPrefix(context, "kind-agentrally-e2e-") {
		return errors.New("e2e requires the driver's disposable kind context")
	}
	return nil
}
