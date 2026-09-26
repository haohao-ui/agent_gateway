package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agent-gateway/internal/httpapi"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/protocol"
)

// Sentinel errors a caller can branch on. Transport failures and 5xx responses
// are deliberately not sentinels: they are transient and the caller retries.
var (
	ErrUnauthorized = errors.New("gateway rejected the node credentials")
	ErrConflict     = errors.New("gateway reported a conflict")
	ErrNotFound     = errors.New("task not found")
	ErrRejected     = errors.New("gateway rejected the request")
)

// claimRequestTimeout bounds one long poll. It must exceed the gateway's own
// hold time so a normal 204 arrives as a response rather than as a timeout.
const claimRequestTimeout = 60 * time.Second

// gatewayError is a non-2xx answer from the gateway.
type gatewayError struct {
	Status  int
	Code    string
	Message string
	kind    error
}

func (e *gatewayError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("gateway returned %d", e.Status)
	}
	return fmt.Sprintf("gateway returned %d (%s): %s", e.Status, e.Code, e.Message)
}

func (e *gatewayError) Unwrap() error { return e.kind }

// GatewayClient talks to one gateway over HTTP/2 with the node's certificate.
type GatewayClient struct {
	base *url.URL
	http *http.Client
}

// NewGatewayClient builds an mTLS client. The certificate is the node identity:
// the gateway derives the actor from it, never from the request body.
func NewGatewayClient(serverURL string, caPEM, certPEM, keyPEM []byte, connectTimeout time.Duration) (*GatewayClient, error) {
	base, err := url.Parse(strings.TrimRight(serverURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse server url: %w", err)
	}
	if base.Scheme != "https" {
		return nil, fmt.Errorf("%w: server url must use https, got %q", protocol.ErrInvalid, base.Scheme)
	}

	tlsConfig, err := identity.BuildClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}

	transport := &http.Transport{
		TLSClientConfig:   tlsConfig,
		ForceAttemptHTTP2: true,
		DialContext: (&net.Dialer{
			Timeout:   connectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: connectTimeout,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		// No ResponseHeaderTimeout: a claim long poll is expected to hold the
		// response open. Each call carries its own context deadline instead.
	}

	return &GatewayClient{base: base, http: &http.Client{Transport: transport}}, nil
}

// CloseIdleConnections releases pooled connections.
func (c *GatewayClient) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

// Claim asks for the next queued task. A nil lease with a nil error means the
// gateway had nothing to hand over (204) and the caller should poll again.
func (c *GatewayClient) Claim(ctx context.Context, leaseSeconds int) (*protocol.Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, claimRequestTimeout)
	defer cancel()

	var lease protocol.Lease
	status, err := c.do(ctx, http.MethodPost, "/v1/tasks/claim", protocol.ClaimRequest{LeaseDurationSeconds: leaseSeconds}, &lease, http.StatusOK, http.StatusNoContent)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &lease, nil
}

// Start confirms the node recorded the lease locally and is about to launch the
// work. It is safe to repeat: the gateway confirms a retransmission.
func (c *GatewayClient) Start(ctx context.Context, lease *protocol.Lease) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/tasks/start", httpapi.StartRequest{
		TaskID:    lease.Task.ID,
		AttemptID: lease.Task.AttemptID,
		Token:     lease.Token,
	}, nil, http.StatusOK)
	return err
}

// Renew extends the lease and returns the deadline the gateway recorded.
func (c *GatewayClient) Renew(ctx context.Context, lease *protocol.Lease, leaseSeconds int) (time.Time, error) {
	var resp protocol.RenewResponse
	if _, err := c.do(ctx, http.MethodPost, "/v1/tasks/renew", protocol.RenewRequest{
		TaskID:               lease.Task.ID,
		AttemptID:            lease.Task.AttemptID,
		Token:                lease.Token,
		LeaseDurationSeconds: leaseSeconds,
	}, &resp, http.StatusOK); err != nil {
		return time.Time{}, err
	}
	return resp.LeaseExpiresAt, nil
}

// Complete reports the terminal result. The gateway treats a retransmission of
// the same result as success, so a lost acknowledgement is recoverable. It
// takes the credential fields rather than a lease because a stored result is
// reported again from the outbox after a restart, when no lease object exists.
func (c *GatewayClient) Complete(ctx context.Context, taskID, attemptID, token string, result protocol.Result) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/tasks/complete", protocol.CompleteRequest{
		TaskID:    taskID,
		AttemptID: attemptID,
		Token:     token,
		Result:    result,
	}, nil, http.StatusOK)
	return err
}

// Cancel asks the gateway to stop a task owned by this node. The gateway
// records the request; the running attempt is what actually stops the work.
func (c *GatewayClient) Cancel(ctx context.Context, taskID string) (protocol.Task, error) {
	var task protocol.Task
	if _, err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/cancel", nil, &task, http.StatusOK); err != nil {
		return protocol.Task{}, err
	}
	return task, nil
}

// Get reads one task owned by this node.
func (c *GatewayClient) Get(ctx context.Context, taskID string) (protocol.Task, error) {
	var task protocol.Task
	if _, err := c.do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID), nil, &task, http.StatusOK); err != nil {
		return protocol.Task{}, err
	}
	return task, nil
}

// Submit queues work for this node. The gateway requires the certificate
// identity and the body's node_id to agree.
func (c *GatewayClient) Submit(ctx context.Context, req protocol.SubmitRequest) (protocol.Task, error) {
	var task protocol.Task
	if _, err := c.do(ctx, http.MethodPost, "/v1/tasks/submit", req, &task, http.StatusCreated); err != nil {
		return protocol.Task{}, err
	}
	return task, nil
}

// do performs one JSON request and decodes the response into dst when the
// status is one of want. It classifies failures so the caller can tell a
// transient transport problem from a refusal that retrying cannot fix.
func (c *GatewayClient) do(ctx context.Context, method, path string, payload, dst any, want ...int) (int, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Cancellation is the caller's decision, not a gateway failure.
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("call gateway: %w", err)
	}
	defer resp.Body.Close()

	for _, status := range want {
		if resp.StatusCode == status {
			// 204 is the gateway's "nothing to hand over" answer to a claim long
			// poll. It carries no body by definition, so decoding it would turn
			// the normal idle outcome into a bogus error and an endless retry
			// loop, which also defeats the wake-up: the gateway can only wake a
			// node that is parked in a poll, not one sleeping in a backoff.
			if dst == nil || resp.StatusCode == http.StatusNoContent {
				_, _ = io.Copy(io.Discard, resp.Body)
				return status, nil
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst); err != nil {
				return status, fmt.Errorf("decode %s response: %w", path, err)
			}
			return status, nil
		}
	}

	return resp.StatusCode, classify(resp)
}

// classify turns a refusal into an error the caller can branch on. Only the
// gateway's stable error code and status are kept: response bodies are never
// echoed back into logs, because they can carry task data.
func classify(resp *http.Response) error {
	var payload protocol.ErrorResponse
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = json.Unmarshal(raw, &payload)

	gwErr := &gatewayError{Status: resp.StatusCode, Code: payload.ErrorCode, Message: payload.Message}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		gwErr.kind = ErrUnauthorized
	case http.StatusNotFound:
		gwErr.kind = ErrNotFound
	case http.StatusConflict:
		gwErr.kind = ErrConflict
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		gwErr.kind = ErrRejected
	}
	if resp.StatusCode >= 500 {
		gwErr.kind = nil
	}
	return gwErr
}

// newTLSClient is a small helper for the pairing client, which has no node
// certificate yet.
func newTLSClient(tlsConfig *tls.Config, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     tlsConfig,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: timeout,
		},
		Timeout: timeout,
	}
}
