package connector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"time"
)

type invocationStream interface {
	write(InvocationFrame) error
	finish() error
	abort()
}

type invocationForwarder struct {
	resolve func(context.Context, InvocationIdentity) (invocationTarget, error)
	stream  func(context.Context, InvocationCommand) (invocationStream, error)
}

// forward never retries an application request. Even a dial failure after the
// dispatch boundary is reported conservatively as an unknown outcome.
func (f invocationForwarder) forward(ctx context.Context, command InvocationCommand, dispatch func() error) InvocationResult {
	result := InvocationResult{RequestID: command.RequestID, ExecutionID: command.ExecutionID, Delivery: "NotDispatched"}
	target, err := f.resolve(ctx, command.InvocationIdentity)
	if err != nil {
		result.Reason = invocationReason(err)
		return result
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	method, path := http.MethodPost, "/start"
	if command.Operation == "Ping" {
		method, path = http.MethodGet, "/ping"
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+target.Host+path, bytes.NewReader(command.Body))
	if err != nil {
		result.Reason = "InvalidTarget"
		return result
	}
	// No replayable body, redirects, environment proxy, or connection reuse.
	req.GetBody = nil
	for name, value := range command.Headers {
		req.Header.Set(name, value)
	}
	if command.Operation == "Start" {
		req.Header.Set("Content-Type", command.ContentType)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 32 << 10,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, target.Address)
		}}
	defer transport.CloseIdleConnections()
	if err := dispatch(); err != nil {
		result.Reason = invocationReason(err)
		return result
	}
	result.Delivery = "OutcomeUnknown"
	response, err := transport.RoundTrip(req)
	if err != nil {
		result.Reason = forwardFailure(ctx, err)
		return result
	}
	defer func() { _ = response.Body.Close() }()
	result.StatusCode, result.Headers = response.StatusCode, invocationResponseHeaders(response.Header)
	idle := time.AfterFunc(60*time.Second, func() { cancel(invocationError("UpstreamIdleTimeout")) })
	defer idle.Stop()
	reader := &invocationReader{reader: response.Body, idle: idle}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		if command.Operation != "Start" {
			result.Reason = "UnexpectedStream"
			return result
		}
		err = f.forwardSSE(ctx, command, response, reader)
	} else {
		result.Body, err = io.ReadAll(io.LimitReader(reader, MaxInvocationResponse+1))
		if len(result.Body) > MaxInvocationResponse {
			result.Body = nil
			err = invocationError("ResponseTooLarge")
		}
	}
	if err != nil {
		result.Body = nil
		result.Reason = forwardFailure(ctx, err)
		return result
	}
	if _, err := f.resolve(ctx, command.InvocationIdentity); err != nil {
		result.Body = nil
		result.Reason = "ExecutionChanged"
		return result
	}
	result.Delivery, result.Reason = "ResponseReceived", "ResponseComplete"
	return result
}

type invocationReader struct {
	reader io.Reader
	idle   *time.Timer
}

func (r *invocationReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if n > 0 {
		r.idle.Reset(60 * time.Second)
	}
	return n, err
}

func invocationResponseHeaders(headers http.Header) map[string]string {
	result := map[string]string{}
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "Traceparent"} {
		if value := headers.Get(name); value != "" && len(value) <= 4096 {
			result[name] = value
		}
	}
	return result
}

func forwardFailure(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return invocationReason(cancellationReason(ctx))
	}
	if reason, ok := err.(invocationError); ok {
		return string(reason)
	}
	return "UpstreamError"
}

func (f invocationForwarder) forwardSSE(ctx context.Context, command InvocationCommand, response *http.Response, reader io.Reader) error {
	stream, err := f.stream(ctx, command)
	if err != nil {
		return err
	}
	defer stream.abort()
	sequence := uint64(0)
	write := func(frame InvocationFrame) error {
		frame.RequestID, frame.ExecutionID, frame.Sequence = command.RequestID, command.ExecutionID, sequence
		sequence++
		return stream.write(frame)
	}
	if err := write(InvocationFrame{Type: "headers", StatusCode: response.StatusCode, Headers: invocationResponseHeaders(response.Header)}); err != nil {
		return err
	}
	buffer := make([]byte, MaxStreamChunk)
	var accounting sseEventAccounting
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if err := accounting.accept(buffer[:n]); err != nil {
				readErr = err
			} else if err := write(InvocationFrame{Type: "data", Data: buffer[:n]}); err != nil {
				return err
			}
		}
		if readErr == nil {
			continue
		}
		reason := "EOF"
		if !errors.Is(readErr, io.EOF) {
			reason = forwardFailure(ctx, readErr)
			if reason != "Cancelled" && reason != "DeadlineExceeded" && reason != "Backpressure" {
				reason = "UpstreamError"
			}
		}
		if err := write(InvocationFrame{Type: "end", Reason: reason}); err != nil {
			return err
		}
		if err := stream.finish(); err != nil {
			return err
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		return readErr
	}
}

// Count SSE event bytes without parsing or retaining application content.
// CR, LF, and CRLF delimiters may cross read boundaries.
type sseEventAccounting struct {
	size, line int
	cr         bool
}

func (s *sseEventAccounting) accept(data []byte) error {
	endLine := func() {
		if s.line == 0 {
			s.size = 0
		}
		s.line = 0
	}
	for _, b := range data {
		if s.cr {
			s.cr = false
			if b == '\n' {
				s.size++
				if s.size > MaxSSEEvent {
					return invocationError("SSEEventTooLarge")
				}
				endLine()
				continue
			}
			endLine()
		}
		s.size++
		if s.size > MaxSSEEvent {
			return invocationError("SSEEventTooLarge")
		}
		switch b {
		case '\r':
			s.cr = true
		case '\n':
			endLine()
		default:
			s.line++
		}
	}
	return nil
}
