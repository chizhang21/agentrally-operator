package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func invocationCommand() InvocationCommand {
	now := time.Now().UTC()
	return InvocationCommand{InvocationIdentity: InvocationIdentity{Scope: RequestScope{GroupName: "team", GroupUID: "group-id", NamespaceUID: "namespace-id"},
		InstanceID: "agent", InstanceUID: "instance-id", ExecutionID: "execution-id"}, ConnectionEpoch: "epoch", RequestID: "request", GateEpoch: 1,
		Operation: "Start", IssuedAt: now, ExpiresAt: now.Add(time.Minute), ContentType: "application/json", Body: json.RawMessage(`{"hello":"world"}`)}
}

func gateCommand(command InvocationCommand, action string, epoch uint64) InvocationControl {
	return InvocationControl{InvocationIdentity: command.InvocationIdentity, ID: fmt.Sprintf("%s-%s-%d", command.InstanceUID, action, epoch), Kind: "Gate", ConnectionEpoch: command.ConnectionEpoch,
		GateEpoch: epoch, Action: action, IssuedAt: command.IssuedAt, ExpiresAt: command.ExpiresAt}
}

func receiveResult(t *testing.T, results <-chan InvocationResult) InvocationResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("result timed out")
		return InvocationResult{}
	}
}

func TestInvocationGateCancellationAndDuplicateDispatch(t *testing.T) {
	s := newInvocationSession(context.Background(), "epoch")
	defer s.close()
	results := make(chan InvocationResult, 10)
	dispatched := make(chan struct{}, 10)
	var calls atomic.Int32
	s.resolve = func(context.Context, InvocationIdentity) (invocationTarget, error) { return invocationTarget{}, nil }
	s.ack = func(_ context.Context, r InvocationResult) error { results <- r; return nil }
	s.forward = func(ctx context.Context, c InvocationCommand, dispatch func() error) InvocationResult {
		r := InvocationResult{RequestID: c.RequestID, Delivery: "NotDispatched"}
		if err := dispatch(); err != nil {
			r.Reason = invocationReason(err)
			return r
		}
		calls.Add(1)
		dispatched <- struct{}{}
		<-ctx.Done()
		r.Delivery = "OutcomeUnknown"
		r.Reason = invocationReason(cancellationReason(ctx))
		return r
	}
	c := invocationCommand()
	if err := s.submit(c); err != nil {
		t.Fatal(err)
	}
	if r := receiveResult(t, results); r.Delivery != "NotDispatched" || r.Reason != "GateClosed" {
		t.Fatal(r)
	}
	c.RequestID = "active"
	if result := s.control(context.Background(), gateCommand(c, "Open", 1)); result.Status != 200 {
		t.Fatal(result)
	}
	if err := s.submit(c); err != nil {
		t.Fatal(err)
	}
	<-dispatched
	if err := s.submit(c); err != nil {
		t.Fatal(err)
	}
	conflict := c
	conflict.Body = json.RawMessage(`"different"`)
	if err := s.submit(conflict); err != invocationError("RequestConflict") {
		t.Fatal(err)
	}
	closed := s.control(context.Background(), gateCommand(c, "Close", 2))
	if closed.Status != 200 || closed.ActiveStarts != 1 || closed.DrainDeadline == nil {
		t.Fatal(closed)
	}
	cancel := InvocationControl{InvocationIdentity: c.InvocationIdentity, ID: "cancel", Kind: "Cancel", ConnectionEpoch: "epoch", RequestID: c.RequestID,
		IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt, Reason: "ClientCancelled"}
	bad := cancel
	bad.ExecutionID = "other"
	bad.ID = "bad"
	if r := s.control(context.Background(), bad); r.Status != 409 {
		t.Fatal(r)
	}
	if r := s.control(context.Background(), cancel); r.Status != 200 {
		t.Fatal(r)
	}
	if r := receiveResult(t, results); r.Delivery != "OutcomeUnknown" || r.Reason != "Cancelled" {
		t.Fatal(r)
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate dispatched %d times", calls.Load())
	}
	// Early cancellation is bound to both identity and the original deadline.
	c.RequestID = "late"
	cancel.ID, cancel.RequestID = "early", c.RequestID
	if r := s.control(context.Background(), cancel); r.Status != 200 {
		t.Fatal(r)
	}
	if err := s.submit(c); err != nil {
		t.Fatal(err)
	}
	if r := receiveResult(t, results); r.Delivery != "NotDispatched" || r.Reason != "Cancelled" {
		t.Fatal(r)
	}
}

func TestInvocationPingBudgetAndHistoryBound(t *testing.T) {
	s := newInvocationSession(context.Background(), "epoch")
	defer s.close()
	results := make(chan InvocationResult, 100)
	s.ack = func(_ context.Context, r InvocationResult) error { results <- r; return nil }
	s.resolve = func(context.Context, InvocationIdentity) (invocationTarget, error) { return invocationTarget{}, nil }
	started := make(chan string, 20)
	s.forward = func(ctx context.Context, c InvocationCommand, dispatch func() error) InvocationResult {
		if err := dispatch(); err != nil {
			return InvocationResult{Reason: invocationReason(err)}
		}
		started <- c.Operation
		if c.Operation == "Start" {
			<-ctx.Done()
		}
		return InvocationResult{RequestID: c.RequestID, Delivery: "ResponseReceived"}
	}
	for i := range 16 {
		c := invocationCommand()
		c.RequestID = fmt.Sprintf("req-%d", i)
		c.InstanceUID = fmt.Sprintf("instance-%d", i)
		c.Scope.GroupUID = fmt.Sprintf("group-%d", i/8)
		if r := s.control(context.Background(), gateCommand(c, "Open", 1)); r.Status != 200 {
			// IDs are unique across a session, even for different instances.
			t.Fatal(r)
		}
		if err := s.submit(c); err != nil {
			t.Fatal(err)
		}
		<-started
	}
	ping := invocationCommand()
	ping.Operation = "Ping"
	ping.GateEpoch = 0
	ping.Body = nil
	ping.ExpiresAt = ping.IssuedAt.Add(5 * time.Second)
	if err := s.submit(ping); err != nil {
		t.Fatal(err)
	}
	select {
	case operation := <-started:
		if operation != "Ping" {
			t.Fatal(operation)
		}
	case <-time.After(time.Second):
		t.Fatal("Ping starved behind Starts")
	}
	if r := receiveResult(t, results); r.RequestID != ping.RequestID {
		t.Fatal(r)
	}
	s.mu.Lock()
	for i := len(s.records); i < maxInvocationHistory; i++ {
		s.records[fmt.Sprintf("past-%d", i)] = &invocationRecord{}
	}
	s.mu.Unlock()
	ping.RequestID = "over-limit"
	if err := s.submit(ping); err != invocationError("SessionHistoryFull") {
		t.Fatal(err)
	}
}

func TestInvocationRejectsUnsafeEnvelopes(t *testing.T) {
	for _, change := range []func(*InvocationCommand){
		func(c *InvocationCommand) { c.Headers = map[string]string{"Authorization": "secret"} },
		func(c *InvocationCommand) { c.Headers = map[string]string{"Accept": "a", "accept": "b"} },
		func(c *InvocationCommand) { c.Headers = map[string]string{"Traceparent": "a\r\nb"} },
		func(c *InvocationCommand) { c.ExpiresAt = c.IssuedAt.Add(301 * time.Second) },
		func(c *InvocationCommand) { c.Body = json.RawMessage(`[]`) },
		func(c *InvocationCommand) { c.Body = bytes.Repeat([]byte("x"), MaxInvocationBody+1) },
		func(c *InvocationCommand) { c.ConnectionEpoch = "old" },
	} {
		c := invocationCommand()
		change(&c)
		if validateInvocation(c, "epoch", time.Now()) == nil {
			t.Fatalf("unsafe command accepted: %s", c.RequestID)
		}
	}
	var c InvocationCommand
	if decodeInvocationJSON([]byte(`{"url":"http://attacker"}`), &c) == nil || decodeInvocationJSON([]byte(`{} garbage`), &c) == nil {
		t.Fatal("unknown or trailing input accepted")
	}
}

func TestRejectedInvocationACKsRemainBounded(t *testing.T) {
	s := newInvocationSession(context.Background(), "epoch")
	defer s.close()
	acked := make(chan struct{}, maxInvocationQueue)
	s.ack = func(ctx context.Context, _ InvocationResult) error {
		acked <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	for i := range maxInvocationQueue {
		c := invocationCommand()
		c.RequestID = fmt.Sprintf("closed-%d", i)
		c.Body = json.RawMessage(`"` + strings.Repeat("x", MaxInvocationBody-2) + `"`)
		if err := s.submit(c); err != nil {
			t.Fatal(err)
		}
		<-acked
	}
	s.mu.Lock()
	queued := s.queued
	retained := 0
	for _, record := range s.records {
		retained += len(record.command.Body)
	}
	s.mu.Unlock()
	if queued != maxInvocationQueue || retained != 0 {
		t.Fatalf("unbounded rejected work: queued=%d retained=%d", queued, retained)
	}
	c := invocationCommand()
	if err := s.submit(c); err != invocationError("InvocationQueueFull") {
		t.Fatal(err)
	}
	if err := s.submit(c); err != nil {
		t.Fatal("queue refusal was not remembered", err)
	}
	if r := s.control(context.Background(), gateCommand(c, "Close", 2)); r.Status != 200 {
		t.Fatal(r)
	}
}

func TestInvocationLocalDeadlineCannotBeExtendedByClockSkew(t *testing.T) {
	s := newInvocationSession(context.Background(), "epoch")
	defer s.close()
	s.ack = func(ctx context.Context, _ InvocationResult) error { <-ctx.Done(); return ctx.Err() }
	s.forward = func(ctx context.Context, c InvocationCommand, _ func() error) InvocationResult {
		<-ctx.Done()
		return InvocationResult{RequestID: c.RequestID}
	}
	c := invocationCommand()
	c.Operation, c.GateEpoch, c.Body = "Ping", 0, nil
	c.IssuedAt = time.Now().UTC().Add(4 * time.Second)
	c.ExpiresAt = c.IssuedAt.Add(5 * time.Second)
	before := time.Now()
	if err := s.submit(c); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	deadline, _ := s.records[c.RequestID].ctx.Deadline()
	s.mu.Unlock()
	if deadline.Sub(before) > 5100*time.Millisecond {
		t.Fatal("clock skew expanded the local Ping budget", deadline.Sub(before))
	}
}

func TestInvocationHTTPPreservesBytesWithoutCredentialsOrRetries(t *testing.T) {
	var calls atomic.Int32
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/start" || r.Header.Get("Authorization") != "" || r.Header.Get("Idempotency-Key") != "key" {
			t.Error("incorrect application request")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"hello":"world"}` {
			t.Errorf("body %q", body)
		}
		w.Header().Set("Set-Cookie", "private=secret")
		w.Header().Set("Location", "http://must-not-follow.invalid")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(302)
		_, _ = w.Write([]byte{0, 1, 2, 255})
	}))
	defer app.Close()
	resolve := func(context.Context, InvocationIdentity) (invocationTarget, error) {
		return invocationTarget{Address: strings.TrimPrefix(app.URL, "http://"), Host: "agent.example:8080"}, nil
	}
	f := invocationForwarder{resolve: resolve}
	c := invocationCommand()
	c.Headers = map[string]string{"Idempotency-Key": "key"}
	r := f.forward(context.Background(), c, func() error { return nil })
	if r.Delivery != "ResponseReceived" || r.StatusCode != 302 || !bytes.Equal(r.Body, []byte{0, 1, 2, 255}) || r.Headers["Set-Cookie"] != "" || calls.Load() != 1 {
		t.Fatalf("unsafe forwarding: %+v, calls=%d", r, calls.Load())
	}
	reads := 0
	f.resolve = func(ctx context.Context, i InvocationIdentity) (invocationTarget, error) {
		reads++
		if reads > 1 {
			return invocationTarget{}, invocationError("ExecutionChanged")
		}
		return resolve(ctx, i)
	}
	r = f.forward(context.Background(), c, func() error { return nil })
	if r.Delivery != "OutcomeUnknown" || r.Reason != "ExecutionChanged" || len(r.Body) != 0 {
		t.Fatal(r)
	}
}

type collectingStream struct {
	frames []InvocationFrame
	fail   bool
}

func (s *collectingStream) write(f InvocationFrame) error {
	if s.fail {
		return invocationError("Backpressure")
	}
	f.Data = bytes.Clone(f.Data)
	s.frames = append(s.frames, f)
	return nil
}
func (s *collectingStream) finish() error { return nil }
func (s *collectingStream) abort()        {}

func TestInvocationSSEIsOpaqueBoundedAndCancellable(t *testing.T) {
	payload := []byte("event: custom\r\ndata: hello\r\n\r\n: comment\n\ndata: last\n\n")
	for _, scenario := range []string{"opaque", "large-event", "backpressure"} {
		t.Run(scenario, func(t *testing.T) {
			stream := &collectingStream{fail: scenario == "backpressure"}
			body := payload
			if scenario == "large-event" {
				body = bytes.Repeat([]byte("x"), MaxSSEEvent+1)
			}
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(body)
			}))
			defer app.Close()
			f := invocationForwarder{resolve: func(context.Context, InvocationIdentity) (invocationTarget, error) {
				return invocationTarget{Address: strings.TrimPrefix(app.URL, "http://"), Host: "agent:8080"}, nil
			},
				stream: func(context.Context, InvocationCommand) (invocationStream, error) { return stream, nil }}
			result := f.forward(context.Background(), invocationCommand(), func() error { return nil })
			if scenario != "opaque" {
				if result.Delivery != "OutcomeUnknown" {
					t.Fatal(result)
				}
				return
			}
			var got []byte
			for i, frame := range stream.frames {
				if frame.Sequence != uint64(i) || len(frame.Data) > MaxStreamChunk {
					t.Fatal(frame)
				}
				got = append(got, frame.Data...)
			}
			if result.Delivery != "ResponseReceived" || !bytes.Equal(got, payload) || stream.frames[len(stream.frames)-1].Reason != "EOF" {
				t.Fatal(result, string(got))
			}
		})
	}
}

func TestInvocationHTTPBoundsAndCancellationKeepOutcomeUnknown(t *testing.T) {
	for _, scenario := range []string{"oversize", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			started := make(chan struct{})
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				if scenario == "cancel" {
					<-r.Context().Done()
					return
				}
				_, _ = w.Write(bytes.Repeat([]byte("x"), MaxInvocationResponse+1))
			}))
			defer app.Close()
			f := invocationForwarder{resolve: func(context.Context, InvocationIdentity) (invocationTarget, error) {
				return invocationTarget{Address: strings.TrimPrefix(app.URL, "http://"), Host: "agent:8080"}, nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancel" {
				go func() { <-started; cancel() }()
			}
			result := f.forward(ctx, invocationCommand(), func() error { return nil })
			if result.Delivery != "OutcomeUnknown" || len(result.Body) != 0 {
				t.Fatal(result)
			}
			want := "ResponseTooLarge"
			if scenario == "cancel" {
				want = "Cancelled"
			}
			if result.Reason != want {
				t.Fatal(result)
			}
		})
	}
}

func TestSSEEventBoundIncludesFragmentedCRLF(t *testing.T) {
	var accounting sseEventAccounting
	for range 2 {
		if err := accounting.accept(bytes.Repeat([]byte("x"), MaxSSEEvent-4)); err != nil {
			t.Fatal(err)
		}
		for _, part := range []string{"\r", "\n", "\r", "\n"} {
			if err := accounting.accept([]byte(part)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := accounting.accept(bytes.Repeat([]byte("x"), MaxSSEEvent-3)); err != nil {
		t.Fatal(err)
	}
	if err := accounting.accept([]byte("\r\n\r")); err != nil {
		t.Fatal(err)
	}
	if err := accounting.accept([]byte("\n")); err != invocationError("SSEEventTooLarge") {
		t.Fatal("final fragmented byte bypassed event limit", err)
	}
}
