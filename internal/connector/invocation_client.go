package connector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

const invocationEpochHeader = "X-Connector-Epoch"

type invocationConnection struct {
	client       *Client
	epoch, token string
}

func (c *Client) setReady(ready bool) {
	c.invocationMu.Lock()
	defer c.invocationMu.Unlock()
	c.ready.Store(ready)
	if !ready && c.invocationCancel != nil {
		c.invocationCancel()
	}
}

func (c *Client) runInvocations(ctx context.Context) error {
	seen := map[string]bool{}
	for ctx.Err() == nil {
		if !c.Ready() {
			if !waitContext(ctx, time.Second) {
				return nil
			}
			continue
		}
		sessionCtx, cancel := context.WithCancel(ctx)
		c.invocationMu.Lock()
		c.invocationCancel = cancel
		if !c.Ready() {
			cancel()
		}
		c.invocationMu.Unlock()
		connection, err := c.invocationHandshake(sessionCtx)
		if err == nil {
			// Stop only the preview on exhaustion or replay. Its supervisor latches
			// the failure until restart, preserving bounded anti-replay history.
			if seen[connection.epoch] || len(seen) >= 64 {
				cancel()
				return invocationError("SessionEpochHistoryInvalid")
			}
			seen[connection.epoch] = true
			connection.run(sessionCtx)
		}
		cancel()
		if !waitContext(ctx, time.Second) {
			return nil
		}
	}
	return nil
}

func (c *Client) invocationHandshake(ctx context.Context) (*invocationConnection, error) {
	token, err := loadToken(c.config.TokenFile)
	if err != nil {
		return nil, err
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(nonceBytes)
	body := mustJSON(map[string]string{"protocol": InvocationProtocol, "nonce": nonce})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.invocations+"/sessions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || response.StatusCode != http.StatusOK {
		return nil, invocationError("HandshakeFailed")
	}
	var session struct {
		Protocol        string `json:"protocol"`
		Nonce           string `json:"nonce"`
		ConnectionEpoch string `json:"connectionEpoch"`
	}
	if decodeInvocationJSON(data, &session) != nil || session.Protocol != InvocationProtocol || session.Nonce != nonce || !wireID.MatchString(session.ConnectionEpoch) {
		return nil, invocationError("InvalidHandshake")
	}
	return &invocationConnection{client: c, epoch: session.ConnectionEpoch, token: token}, nil
}

func (connection *invocationConnection) run(ctx context.Context) {
	session := newInvocationSession(ctx, connection.epoch)
	session.resolve = func(ctx context.Context, identity InvocationIdentity) (invocationTarget, error) {
		return connection.client.executor.resolveInvocation(ctx, identity, connection.client.config.InvocationPeer)
	}
	forwarder := invocationForwarder{resolve: session.resolve, stream: connection.openStream}
	session.forward = forwarder.forward
	session.ack = func(ctx context.Context, result InvocationResult) error {
		return connection.ack(ctx, "/invocations/"+result.RequestID+"/result", mustJSON(result), true)
	}
	done := make(chan struct{}, 3)
	go func() { defer func() { session.cancel(); done <- struct{}{} }(); connection.pollInvocations(session) }()
	go func() { defer func() { session.cancel(); done <- struct{}{} }(); connection.pollControls(session) }()
	go func() {
		defer func() { session.cancel(); done <- struct{}{} }()
		for waitContext(session.ctx, time.Second) {
			token, err := loadToken(connection.client.config.TokenFile)
			if err != nil || token != connection.token || !connection.client.Ready() {
				return
			}
		}
	}()
	<-session.ctx.Done()
	session.close()
	for range 3 {
		<-done
	}
}

func (connection *invocationConnection) request(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Request, error) {
	token, err := loadToken(connection.client.config.TokenFile)
	if err != nil || token != connection.token {
		return nil, invocationError("CredentialChanged")
	}
	req, err := http.NewRequestWithContext(ctx, method, connection.client.invocations+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+connection.token)
	req.Header.Set(invocationEpochHeader, connection.epoch)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func (connection *invocationConnection) poll(ctx context.Context, path string, limit int) ([]byte, error) {
	req, err := connection.request(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	response, err := connection.client.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent {
		if !waitContext(ctx, 100*time.Millisecond) {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit+1)))
	if err != nil || len(data) > limit || response.StatusCode != http.StatusOK {
		return nil, invocationError("PollFailed")
	}
	return data, nil
}

func (connection *invocationConnection) pollInvocations(session *invocationSession) {
	for session.ctx.Err() == nil {
		data, err := connection.poll(session.ctx, "/invocations", MaxInvocationBody+32<<10)
		if err != nil {
			return
		}
		if data == nil {
			continue
		}
		var command InvocationCommand
		if decodeInvocationJSON(data, &command) != nil {
			return
		}
		if err := session.submit(command); err != nil {
			// Malformed/conflicting envelopes invalidate the session. A full queue
			// has a bounded synchronous refusal, independent of the controls worker.
			if err != invocationError("InvocationQueueFull") {
				return
			}
			ackCtx, cancel := context.WithTimeout(session.ctx, 30*time.Second)
			err = connection.ack(ackCtx, "/invocations/"+command.RequestID+"/result", mustJSON(InvocationResult{
				RequestID: command.RequestID, ExecutionID: command.ExecutionID, Delivery: "NotDispatched", Reason: "InvocationQueueFull"}), true)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (connection *invocationConnection) pollControls(session *invocationSession) {
	for session.ctx.Err() == nil {
		data, err := connection.poll(session.ctx, "/controls", 16<<10)
		if err != nil {
			return
		}
		if data == nil {
			continue
		}
		var control InvocationControl
		if decodeInvocationJSON(data, &control) != nil || !wireID.MatchString(control.ID) {
			return
		}
		ctx, cancel := context.WithTimeout(session.ctx, 5*time.Second)
		result := session.control(ctx, control)
		err = connection.ack(ctx, "/controls/"+control.ID+"/result", mustJSON(result), false)
		cancel()
		if err != nil {
			return
		}
	}
}

func (connection *invocationConnection) ack(ctx context.Context, path string, data []byte, retry bool) error {
	if len(data) > MaxInvocationEnvelope {
		return invocationError("ResultTooLarge")
	}
	for {
		req, err := connection.request(ctx, http.MethodPost, path, bytes.NewReader(data), "application/json")
		if err != nil {
			return err
		}
		response, err := connection.client.http.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
			if response.StatusCode >= 400 && response.StatusCode < 500 {
				return invocationError("ResultRejected")
			}
		}
		if !retry || !waitContext(ctx, 200*time.Millisecond) {
			return invocationError("ResultUnacknowledged")
		}
	}
}

type outboundInvocationStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	writer *io.PipeWriter
	done   chan error
}

func (connection *invocationConnection) openStream(ctx context.Context, command InvocationCommand) (invocationStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	req, err := connection.request(ctx, http.MethodPost, "/invocations/"+command.RequestID+"/stream", reader, "application/x-ndjson")
	if err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	stream := &outboundInvocationStream{ctx: ctx, cancel: cancel, writer: writer, done: make(chan error, 1)}
	// A dedicated client removes the management 45s total timeout. The shared
	// transport remains safe for concurrent use; context bounds the whole stream.
	client := *connection.client.http
	client.Timeout = 0
	go func() {
		response, err := client.Do(req)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				err = invocationError("StreamRejected")
			}
		}
		_ = reader.CloseWithError(invocationError("StreamClosed"))
		stream.done <- err
	}()
	return stream, nil
}

func (s *outboundInvocationStream) write(frame InvocationFrame) error {
	// Pipe.Write waits for transport consumption. Only one bounded frame exists;
	// a blocked downstream closes both the pipe and its HTTP request after 5s.
	timer := time.AfterFunc(5*time.Second, func() { _ = s.writer.CloseWithError(invocationError("Backpressure")); s.cancel() })
	defer timer.Stop()
	data, err := json.Marshal(frame)
	if err == nil {
		_, err = s.writer.Write(append(data, '\n'))
	}
	if err != nil {
		if s.ctx.Err() != nil {
			return invocationError("Backpressure")
		}
		return invocationError("StreamClosed")
	}
	return nil
}

func (s *outboundInvocationStream) finish() error {
	_ = s.writer.Close()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-s.done:
		if err != nil {
			return invocationError("StreamUnacknowledged")
		}
		return nil
	case <-s.ctx.Done():
		return cancellationReason(s.ctx)
	case <-timer.C:
		return invocationError("Backpressure")
	}
}

func (s *outboundInvocationStream) abort() { s.cancel(); _ = s.writer.Close() }
