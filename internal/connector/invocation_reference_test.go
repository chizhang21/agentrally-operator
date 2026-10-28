package connector

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This reference consumer deliberately decodes wire fields independently of
// production protocol types. It is a fixture contract, not an Orchestrator.
type referenceFrame struct {
	Request   string `json:"requestId"`
	Execution string `json:"executionId"`
	Sequence  uint64 `json:"sequence"`
	Type      string `json:"type"`
	Data      string `json:"dataBase64"`
	Reason    string `json:"reason"`
}

func referenceFrames(reader io.Reader, request, execution string) ([]byte, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	sequence := uint64(0)
	ended := false
	var data []byte
	for scanner.Scan() {
		var frame referenceFrame
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.Request != request || frame.Execution != execution || frame.Sequence != sequence || ended {
			return nil, fmt.Errorf("invalid frame correlation")
		}
		if sequence == 0 && frame.Type != "headers" || sequence > 0 && frame.Type == "headers" {
			return nil, fmt.Errorf("invalid header sequence")
		}
		switch frame.Type {
		case "headers":
		case "data":
			decoded, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil || len(decoded) > 32<<10 {
				return nil, fmt.Errorf("invalid data")
			}
			data = append(data, decoded...)
		case "end":
			if frame.Reason != "EOF" {
				return nil, fmt.Errorf("incomplete stream")
			}
			ended = true
		default:
			return nil, fmt.Errorf("unknown frame")
		}
		sequence++
	}
	if scanner.Err() != nil || !ended {
		return nil, fmt.Errorf("missing stream end")
	}
	return data, nil
}

type referenceEpoch struct {
	id              string
	closed          chan struct{}
	calls, controls chan []byte
}
type referenceBackend struct {
	t              *testing.T
	mu             sync.Mutex
	epoch          *referenceEpoch
	next           int
	resultHashes   map[string][32]byte
	attempts       map[string]int
	controlResults chan map[string]any
	results        chan map[string]any
	streams        chan []byte
	polled         chan string
	execution      string
}

func newReferenceBackend(t *testing.T) *referenceBackend {
	return &referenceBackend{t: t, resultHashes: map[string][32]byte{}, attempts: map[string]int{}, controlResults: make(chan map[string]any, 10), results: make(chan map[string]any, 10), streams: make(chan []byte, 10), polled: make(chan string, 20)}
}

func (b *referenceBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer credential-one" {
		w.WriteHeader(401)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/sessions") {
		var request map[string]string
		if json.NewDecoder(r.Body).Decode(&request) != nil || request["protocolVersion"] != "invocation.v1" {
			w.WriteHeader(400)
			return
		}
		b.mu.Lock()
		if b.epoch != nil {
			close(b.epoch.closed)
		}
		b.next++
		b.epoch = &referenceEpoch{id: fmt.Sprintf("epoch-%d", b.next), closed: make(chan struct{}), calls: make(chan []byte, 10), controls: make(chan []byte, 10)}
		id := b.epoch.id
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"protocolVersion": "invocation.v1", "nonce": request["nonce"], "connectionEpoch": id})
		return
	}
	b.mu.Lock()
	epoch := b.epoch
	b.mu.Unlock()
	if epoch == nil || r.Header.Get("X-Connector-Epoch") != epoch.id {
		w.WriteHeader(409)
		return
	}
	if r.Method == http.MethodGet {
		queue := epoch.calls
		if strings.HasSuffix(r.URL.Path, "/controls") {
			queue = epoch.controls
		}
		select {
		case b.polled <- epoch.id:
		default:
		}
		select {
		case data := <-queue:
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.epoch != epoch {
				w.WriteHeader(409)
				return
			}
			_, _ = w.Write(data)
		case <-epoch.closed:
			w.WriteHeader(409)
		case <-r.Context().Done():
		case <-time.After(time.Second):
			w.WriteHeader(204)
		}
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	id := parts[len(parts)-2]
	if strings.HasSuffix(r.URL.Path, "/stream") {
		if r.Header.Get("Content-Type") != "application/x-ndjson" {
			w.WriteHeader(400)
			return
		}
		data, err := referenceFrames(r.Body, id, b.execution)
		if err != nil {
			b.t.Error(err)
			w.WriteHeader(400)
			return
		}
		b.streams <- data
		w.WriteHeader(204)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (6<<20)+1))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	var result map[string]any
	if json.Unmarshal(body, &result) != nil {
		w.WriteHeader(400)
		return
	}
	if strings.Contains(r.URL.Path, "/controls/") {
		if result["id"] != id {
			b.t.Error("control correlation mismatch")
		}
		b.controlResults <- result
		w.WriteHeader(204)
		return
	}
	if result["requestId"] != id || result["executionId"] != b.execution {
		b.t.Error("result correlation mismatch")
		w.WriteHeader(400)
		return
	}
	hash := sha256.Sum256(body)
	b.mu.Lock()
	prior, exists := b.resultHashes[id]
	b.resultHashes[id] = hash
	b.attempts[id]++
	attempt := b.attempts[id]
	b.mu.Unlock()
	if exists && prior != hash {
		b.t.Error("result changed on retry")
		w.WriteHeader(409)
		return
	}
	if attempt == 1 {
		w.WriteHeader(503)
		return
	} // Lost result ACK must not replay Start.
	b.results <- result
	w.WriteHeader(204)
}

func referenceClient(t *testing.T, b *referenceBackend) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(b)
	t.Cleanup(server.Close)
	config := testConfig(t)
	config.Endpoint = server.URL
	config.InvocationPreview = true
	config.InvocationPeer = testInvocationPeer
	client, err := NewClient(config, Executor{Client: fakeKube()})
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	client.http.Timeout = 45 * time.Second
	client.setReady(true)
	return client, server
}

func TestSharedInvocationFixtureAndConsumerRejectsBrokenStreams(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/connector/invocation-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Start  InvocationCommand `json:"start"`
		Frames []json.RawMessage `json:"frames"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := validateInvocation(fixture.Start, "fixture-epoch", fixture.Start.IssuedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rows := make([][]byte, len(fixture.Frames))
	for i, frame := range fixture.Frames {
		rows[i] = frame
	}
	stream := bytes.Join(rows, []byte("\n"))
	payload, err := referenceFrames(bytes.NewReader(stream), fixture.Start.RequestID, fixture.Start.ExecutionID)
	if err != nil || string(payload) != "data: hello\n\n" {
		t.Fatal(string(payload), err)
	}
	for _, broken := range [][]byte{bytes.Join(rows[:2], []byte("\n")), bytes.Replace(stream, []byte(`"sequence":1`), []byte(`"sequence":7`), 1), bytes.ReplaceAll(stream, []byte("fixture-execution"), []byte("other-execution"))} {
		if _, err := referenceFrames(bytes.NewReader(broken), fixture.Start.RequestID, fixture.Start.ExecutionID); err == nil {
			t.Fatal("broken stream accepted")
		}
	}
}

func TestReferenceBackendEpochReplacementCannotConsumeNewQueue(t *testing.T) {
	b := newReferenceBackend(t)
	client, _ := referenceClient(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	old, err := client.invocationHandshake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := old.poll(ctx, "/invocations", 4096); done <- err }()
	<-b.polled
	current, err := client.invocationHandshake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.epoch.calls <- []byte(`{"requestId":"new"}`)
	b.mu.Unlock()
	if err := <-done; err == nil {
		t.Fatal("old poll survived epoch replacement")
	}
	data, err := current.poll(ctx, "/invocations", 4096)
	if err != nil || !bytes.Contains(data, []byte("new")) {
		t.Fatal(string(data), err)
	}
}

func TestInvocationPreviewReferenceBackendAndTokenRotation(t *testing.T) {
	// Use a real routable local address because production resolution rejects loopback.
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	ip := ""
	for _, address := range addresses {
		if block, ok := address.(*net.IPNet); ok && block.IP.To4() != nil && !block.IP.IsLoopback() {
			ip = block.IP.String()
			break
		}
	}
	if ip == "" {
		t.Skip("no non-loopback IPv4 for target resolution")
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	app := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/start" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: hello\n\n"))
	}))
	_ = app.Listener.Close()
	app.Listener = listener
	app.Start()
	defer app.Close()
	port, _ := strconv.Atoi(strings.Split(listener.Addr().String(), ":")[1])
	kube, command := invocationFixture(t, ip, int32(port))
	b := newReferenceBackend(t)
	b.execution = command.ExecutionID
	client, _ := referenceClient(t, b)
	client.executor.Client = kube
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := client.invocationHandshake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	command.ConnectionEpoch = connection.epoch
	done := make(chan struct{})
	go func() { defer close(done); connection.run(ctx) }()
	defer func() { cancel(); <-done }()
	b.mu.Lock()
	epoch := b.epoch
	b.mu.Unlock()
	epoch.controls <- mustJSON(gateCommand(command, "Open", 1))
	select {
	case r := <-b.controlResults:
		if r["status"] != float64(200) {
			t.Fatal(r)
		}
	case <-ctx.Done():
		t.Fatal("open timed out")
	}
	epoch.calls <- mustJSON(command)
	select {
	case data := <-b.streams:
		if string(data) != "data: hello\n\n" {
			t.Fatal(string(data))
		}
	case <-ctx.Done():
		t.Fatal("stream timed out")
	}
	select {
	case r := <-b.results:
		if r["delivery"] != "ResponseReceived" {
			t.Fatal(r)
		}
	case <-ctx.Done():
		t.Fatal("result timed out")
	}
	epoch.calls <- mustJSON(command)
	epoch.controls <- mustJSON(gateCommand(command, "Close", 2))
	select {
	case r := <-b.controlResults:
		if r["status"] != float64(200) {
			t.Fatal(r)
		}
	case <-ctx.Done():
		t.Fatal("close timed out")
	}
	if err := os.WriteFile(client.config.TokenFile, []byte("rotated-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("token rotation left invocation session active")
	}
	if calls.Load() != 1 {
		t.Fatalf("Start replayed %d times", calls.Load())
	}
}

func TestInvocationStreamStopsWhenBackendDoesNotRead(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	config := testConfig(t)
	config.Endpoint = server.URL
	client, err := NewClient(config, Executor{})
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	connection := &invocationConnection{client: client, epoch: "epoch", token: "credential-one"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := connection.openStream(ctx, invocationCommand())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.abort()
	frame := InvocationFrame{Type: "data", Data: bytes.Repeat([]byte("x"), 32<<10)}
	started := time.Now()
	for i := range 10000 {
		frame.Sequence = uint64(i)
		if err := stream.write(frame); err != nil {
			if err != invocationError("Backpressure") || time.Since(started) > 10*time.Second {
				t.Fatal("unbounded downstream write", err, time.Since(started))
			}
			return
		}
	}
	t.Fatal("backend did not apply backpressure")
}
