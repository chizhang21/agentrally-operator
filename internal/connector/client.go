package connector

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
)

type Config struct {
	Endpoint  string
	ID        string
	TokenFile string
	CAFile    string
}

// Client runs one serial poll worker on the elected Operator replica.
type Client struct {
	config    Config
	requests  string
	responses string
	http      *http.Client
	executor  Executor
	ready     atomic.Bool
	wait      func(context.Context, time.Duration) bool
}

func NewClient(config Config, executor Executor) (*Client, error) {
	base, err := url.Parse(config.Endpoint)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return nil, fmt.Errorf("connector endpoint must be an HTTPS base URL without credentials, query or fragment")
	}
	if config.ID == "" || len(validation.IsDNS1123Subdomain(config.ID)) != 0 {
		return nil, fmt.Errorf("connector id must be a DNS subdomain")
	}
	if _, err := loadToken(config.TokenFile); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if config.CAFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("cannot load system CA certificates")
		}
		pem, err := os.ReadFile(config.CAFile)
		if err != nil || !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cannot load connector CA certificates")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/internal/v1/connectors/" + config.ID
	base.RawPath = ""
	return &Client{
		config: config, requests: base.String() + "/requests", responses: base.String() + "/responses",
		http:     &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		executor: executor, wait: waitContext,
	}, nil
}

func (c *Client) Ready() bool { return c.ready.Load() }

func (c *Client) NeedLeaderElection() bool { return true }

func (c *Client) Start(ctx context.Context) error {
	defer c.ready.Store(false)
	defer c.http.CloseIdleConnections()
	backoff := time.Second
	for ctx.Err() == nil {
		req, err := c.poll(ctx)
		if err != nil {
			c.ready.Store(false)
			if !c.retry(ctx, &backoff, "poll") {
				return nil
			}
			continue
		}
		c.ready.Store(true)
		backoff = time.Second
		if req == nil {
			// A misconfigured backend returning immediate 204 must not spin.
			if !c.wait(ctx, time.Second) {
				return nil
			}
			continue
		}
		response := c.executor.Execute(ctx, *req)
		data, _ := json.Marshal(response)
		if len(data) > MaxMessageBytes {
			status := apierrors.NewRequestEntityTooLargeError("response exceeds connector limit; use a smaller list page or read the object directly").Status()
			status.APIVersion, status.Kind = "v1", "Status"
			response.Status, response.Body = int(status.Code), mustJSON(status)
			data, _ = json.Marshal(response)
		}
		// Retry only the response. Never rerun the Kubernetes operation here.
		for ctx.Err() == nil {
			if err := c.acknowledge(ctx, data); err != nil {
				c.ready.Store(false)
				if !c.retry(ctx, &backoff, "response") {
					return nil
				}
				continue
			}
			c.ready.Store(true)
			break
		}
	}
	return nil
}

func (c *Client) retry(ctx context.Context, delay *time.Duration, phase string) bool {
	if ctx.Err() != nil {
		return false
	}
	// Never log remote bodies, native objects, URLs or credentials.
	slog.Warn("connector connection unavailable", "phase", phase, "retryAfter", *delay)
	keepGoing := c.wait(ctx, *delay+time.Duration(rand.Int64N(int64(*delay/4))))
	*delay = min(*delay*2, 30*time.Second)
	return keepGoing
}

func (c *Client) poll(ctx context.Context) (*Request, error) {
	response, err := c.send(ctx, http.MethodGet, c.requests, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("poll HTTP status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxMessageBytes+1))
	if err != nil || len(data) > MaxMessageBytes {
		return nil, fmt.Errorf("cannot read bounded request")
	}
	var req Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || req.ID == "" || len(req.ID) > 128 || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid connector request envelope")
	}
	return &req, nil
}

func (c *Client) acknowledge(ctx context.Context, data []byte) error {
	response, err := c.send(ctx, http.MethodPost, c.responses, data)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("response HTTP status %d", response.StatusCode)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method, endpoint string, data []byte) (*http.Response, error) {
	token, err := loadToken(c.config.TokenFile)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func loadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 16*1024 {
		return "", fmt.Errorf("cannot read connector token file")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("connector token file is empty")
	}
	for _, char := range token {
		if char <= 32 || char >= 127 {
			return "", fmt.Errorf("connector token must contain printable ASCII without whitespace")
		}
	}
	return token, nil
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
