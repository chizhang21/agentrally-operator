//go:build integration

package main

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentrally/operator/internal/connector"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestConnectorLeaderHandoffAndReadiness(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("set KUBEBUILDER_ASSETS to envtest binaries")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS")}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "connector-election"}}); err != nil {
		t.Fatal(err)
	}
	var polls [2]atomic.Int32
	var available atomic.Bool
	available.Store(true)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := 0
		if strings.Contains(r.URL.Path, "/replica-two/") {
			index = 1
		}
		polls[index].Add(1)
		if !available.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	dir := t.TempDir()
	token, ca := filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(token, []byte("local-integration-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	var managers [2]ctrl.Manager
	var addresses [2]string
	for index, id := range []string{"replica-one", "replica-two"} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addresses[index] = listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		options := managerOptions(addresses[index], "0", "connector-election", true)
		lease, renew, retry := 5*time.Second, 3*time.Second, 500*time.Millisecond
		options.LeaseDuration, options.RenewDeadline, options.RetryPeriod = &lease, &renew, &retry
		managers[index], err = ctrl.NewManager(cfg, options)
		if err != nil {
			t.Fatal(err)
		}
		if err := addConnector(managers[index], cfg, connector.Config{Endpoint: backend.URL, ID: id, TokenFile: token, CAFile: ca}); err != nil {
			t.Fatal(err)
		}
	}
	var stops [2]context.CancelFunc
	for index, mgr := range managers {
		runCtx, stop := context.WithCancel(ctx)
		stops[index] = stop
		done := make(chan error, 1)
		go func() { done <- mgr.Start(runCtx) }()
		t.Cleanup(func() {
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("manager did not stop its Connector")
			}
		})
	}
	await := func(description string, condition func() bool) {
		t.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) { return condition(), nil }); err != nil {
			t.Fatalf("%s: %v", description, err)
		}
	}
	httpClient := &http.Client{Timeout: time.Second}
	ready := func(index int) bool {
		response, err := httpClient.Get("http://" + addresses[index] + "/readyz/connector")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}
	await("one elected worker polls", func() bool { return polls[0].Load()+polls[1].Load() > 0 })
	leader := 0
	if polls[1].Load() > 0 {
		leader = 1
	}
	standby := 1 - leader
	if polls[standby].Load() != 0 {
		t.Fatal("standby Connector polled before election")
	}
	await("both replicas ready", func() bool { return ready(0) && ready(1) })
	available.Store(false)
	await("only leader reports backend failure", func() bool { return !ready(leader) && ready(standby) })
	if polls[standby].Load() != 0 {
		t.Fatal("standby Connector ran while leader was disconnected")
	}
	available.Store(true)
	stops[leader]()
	await("standby takes over polling", func() bool { return polls[standby].Load() > 0 && ready(standby) })
}
