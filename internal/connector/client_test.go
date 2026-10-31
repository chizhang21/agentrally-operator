package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func remoteResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("credential-one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{Endpoint: "https://backend.example/agentrally/", ID: "cluster-one", TokenFile: path}
}

func TestResponseRetryDoesNotReplayWriteAndReloadsToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := testConfig(t)
	kube := fakeKube()
	client, err := NewClient(config, Executor{Client: kube})
	if err != nil {
		t.Fatal(err)
	}
	polls, acknowledgements, waits := 0, 0, 0
	var firstResponse string
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer credential-one" && acknowledgements < 1 {
			t.Fatal("initial bearer credential missing")
		}
		if req.Method == "GET" {
			polls++
			if polls != 1 || req.URL.Path != "/agentrally/internal/connectors/cluster-one/requests" {
				t.Fatal("polled again before acknowledging executed operation")
			}
			return remoteResponse(req, 200, `{"id":"write-1","method":"POST","resource":"agentgroups","scope":{"groupName":"demo"},"body":{"apiVersion":"arally.io/v1alpha1","kind":"AgentGroup","metadata":{"name":"demo"},"spec":{"namespace":"agentrally-demo"}}}`), nil
		}
		if req.Method != "POST" || req.URL.Path != "/agentrally/internal/connectors/cluster-one/responses" || req.Header.Get("Content-Type") != "application/json" {
			t.Fatal("invalid response endpoint")
		}
		data, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		acknowledgements++
		if acknowledgements == 1 {
			firstResponse = string(data)
			var response Response
			if json.Unmarshal(data, &response) != nil || response.ID != "write-1" || response.Status != 201 {
				t.Fatal("native create response lost")
			}
			if err := os.WriteFile(config.TokenFile, []byte("credential-two"), 0600); err != nil {
				t.Fatal(err)
			}
			return remoteResponse(req, 503, "unavailable"), nil
		}
		if string(data) != firstResponse || req.Header.Get("Authorization") != "Bearer credential-two" {
			t.Fatal("response retry changed result or failed to reload token")
		}
		cancel()
		return remoteResponse(req, 204, ""), nil
	})
	client.wait = func(context.Context, time.Duration) bool {
		waits++
		if client.Ready() {
			t.Fatal("connector advertised readiness during failed acknowledgement")
		}
		return true
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if polls != 1 || acknowledgements != 2 || waits != 1 || len(kube.Actions()) != 1 || kube.Actions()[0].GetVerb() != "create" || client.Ready() {
		t.Fatal("write was replayed or cancellation did not reset readiness")
	}
}

func TestOfflineRetriesAndIdlePollingHasPacing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kube := fakeKube()
	client, err := NewClient(testConfig(t), Executor{Client: kube})
	if err != nil {
		t.Fatal(err)
	}
	polls, waits := 0, 0
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		polls++
		if polls == 1 {
			return nil, errors.New("disconnected")
		}
		return remoteResponse(req, 204, ""), nil
	})
	client.wait = func(_ context.Context, delay time.Duration) bool {
		waits++
		if waits == 1 {
			if client.Ready() || delay < time.Second || delay >= 1250*time.Millisecond {
				t.Fatal("invalid failure readiness/backoff")
			}
			return true
		}
		if !client.Ready() || delay != time.Second {
			t.Fatal("idle polling did not report connectivity or prevent busy loop")
		}
		cancel()
		return false
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if polls != 2 || waits != 2 || len(kube.Actions()) != 0 || client.Ready() {
		t.Fatal("offline polling unexpectedly touched Kubernetes")
	}
}

func TestPollRejectsMalformedAndOversizedEnvelopes(t *testing.T) {
	for name, body := range map[string]string{
		"empty id":            `{"method":"GET","resource":"agentgroups"}`,
		"unknown route field": `{"id":"1","method":"GET","resource":"agentgroups","url":"/api/v1/secrets"}`,
		"explicit intent":     `{"id":"1","method":"PUT","resource":"agentinstances","intent":{"kind":"Explicit"}}`,
		"observed intent":     `{"id":"1","method":"PUT","resource":"agentinstances","intent":{"kind":"Observed","expectedExecutionId":"execution","observedAt":"2026-10-31T00:00:00Z"}}`,
		"trailing object":     `{"id":"1","method":"GET","resource":"agentgroups"}{}`,
		"null":                `null`,
		"oversized":           strings.Repeat(" ", MaxMessageBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			kube := fakeKube()
			client, err := NewClient(testConfig(t), Executor{Client: kube})
			if err != nil {
				t.Fatal(err)
			}
			client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return remoteResponse(req, 200, body), nil
			})
			if _, err := client.poll(context.Background()); err == nil || len(kube.Actions()) != 0 {
				t.Fatal("malformed envelope accepted")
			}
		})
	}
}

func TestCredentialsCannotFollowRedirects(t *testing.T) {
	client, err := NewClient(testConfig(t), Executor{Client: fakeKube()})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "backend.example" {
			t.Fatal("credential request followed redirect")
		}
		response := remoteResponse(req, 302, "")
		response.Header.Set("Location", "https://another.example/collect")
		return response, nil
	})
	if _, err := client.poll(context.Background()); err == nil || calls != 1 {
		t.Fatal("redirect was not rejected")
	}
}

func TestCancellationInterruptsLongPollAndWait(t *testing.T) {
	client, err := NewClient(testConfig(t), Executor{Client: fakeKube()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := client.Start(ctx); err != nil {
			t.Errorf("connector stopped with an error: %v", err)
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop long poll")
	}
	if waitContext(ctx, time.Hour) || client.Ready() {
		t.Fatal("cancelled backoff did not stop")
	}
}

func TestConfigurationRequiresHTTPSIdentityAndCredential(t *testing.T) {
	base := testConfig(t)
	for _, endpoint := range []string{"http://backend.example", "https://user:password@backend.example", "https://backend.example/?token=secret", "https://backend.example/#secret", "https:"} {
		config := base
		config.Endpoint = endpoint
		if _, err := NewClient(config, Executor{}); err == nil {
			t.Fatalf("invalid endpoint accepted: %s", endpoint)
		}
	}
	for _, id := range []string{"", "../other", "cluster/other"} {
		config := base
		config.ID = id
		if _, err := NewClient(config, Executor{}); err == nil {
			t.Fatal("invalid cluster id accepted")
		}
	}
	for _, credential := range []string{"", "embedded\nnewline", "two tokens", "\u0085"} {
		if err := os.WriteFile(base.TokenFile, []byte(credential), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewClient(base, Executor{}); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
}

func TestInvocationWorkerFailureKeepsManagementRunning(t *testing.T) {
	var handshakes, polls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/requests"):
			polls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(req.URL.Path, "/sessions"):
			var request struct {
				Nonce string `json:"nonce"`
			}
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			handshakes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"protocol": InvocationProtocol, "nonce": request.Nonce, "connectionEpoch": "reused-epoch"})
		default:
			// End the first session so a second handshake must reject epoch reuse.
			w.WriteHeader(http.StatusConflict)
		}
	}))
	defer server.Close()
	config := testConfig(t)
	config.Endpoint, config.InvocationPreview, config.InvocationPeer = server.URL, true, testInvocationPeer
	connector, err := NewClient(config, Executor{Client: fakeKube()})
	if err != nil {
		t.Fatal(err)
	}
	connector.http = server.Client()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- connector.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Connector did not stop after manager cancellation")
		}
	})
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !connector.InvocationFailed() {
		select {
		case <-done:
			t.Fatal("invocation preview failure stopped management")
		case <-deadline.C:
			t.Fatal("preview failure was not exposed")
		case <-tick.C:
		}
	}
	previous := polls.Load()
	for polls.Load() <= previous {
		select {
		case <-done:
			t.Fatal("management stopped after preview failure")
		case <-deadline.C:
			t.Fatal("management did not continue after preview failure")
		case <-tick.C:
		}
	}
	if !connector.Ready() || handshakes.Load() != 2 || !connector.InvocationFailed() {
		t.Fatalf("preview failure lost isolation or retried a terminal session: ready=%v handshakes=%d", connector.Ready(), handshakes.Load())
	}
}
