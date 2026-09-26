package mcp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/doctor"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
)

// GatewayBackend abstracts communication with the gateway domain or remote server.
type GatewayBackend interface {
	SubmitTask(ctx context.Context, nodeID, capability string, input []byte, timeoutSec int) (protocol.Task, error)
	GetTask(ctx context.Context, taskID string) (protocol.Task, error)
	CancelTask(ctx context.Context, taskID string) (protocol.Task, error)
	ListDevices(ctx context.Context) ([]devicestore.Device, error)
	Diagnose(ctx context.Context) (doctor.Report, error)
}

// LocalBackend directly invokes store methods within the same process.
type LocalBackend struct {
	Tasks   *taskstore.Store
	Devices *devicestore.Store
	DataDir string
}

func (l *LocalBackend) SubmitTask(ctx context.Context, nodeID, capability string, input []byte, timeoutSec int) (protocol.Task, error) {
	if l.Tasks == nil {
		return protocol.Task{}, errors.New("task store not initialized")
	}
	req := protocol.SubmitRequest{
		NodeID:            nodeID,
		Capability:        capability,
		CapabilityVersion: 1,
		Input:             input,
		TimeoutSeconds:    timeoutSec,
	}
	return l.Tasks.Submit(ctx, req)
}

func (l *LocalBackend) GetTask(ctx context.Context, taskID string) (protocol.Task, error) {
	if l.Tasks == nil {
		return protocol.Task{}, errors.New("task store not initialized")
	}
	return l.Tasks.Get(ctx, taskID)
}

func (l *LocalBackend) CancelTask(ctx context.Context, taskID string) (protocol.Task, error) {
	if l.Tasks == nil {
		return protocol.Task{}, errors.New("task store not initialized")
	}
	return l.Tasks.Cancel(ctx, taskID)
}

func (l *LocalBackend) ListDevices(ctx context.Context) ([]devicestore.Device, error) {
	if l.Devices == nil {
		return nil, errors.New("device store not initialized")
	}
	return l.Devices.List(ctx)
}

func (l *LocalBackend) Diagnose(ctx context.Context) (doctor.Report, error) {
	dir := l.DataDir
	if dir == "" {
		dir = "./gateway-data"
	}
	return doctor.DiagnoseServer(ctx, dir), nil
}

// ClientBackend talks to a running Gateway server via HTTPS REST API with an operator token.
type ClientBackend struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewClientBackend creates a GatewayBackend backed by the gateway REST API.
func NewClientBackend(baseURL, token string, caCertPEM []byte) (*ClientBackend, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
	}
	if len(caCertPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCertPEM) {
			return nil, errors.New("failed to parse gateway CA certificate")
		}
		tlsConfig.RootCAs = pool
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   tlsConfig,
			ForceAttemptHTTP2: true,
		},
		Timeout: 30 * time.Second,
	}

	return &ClientBackend{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Client:  client,
	}, nil
}

func (c *ClientBackend) do(ctx context.Context, method, path string, bodyIn any, respOut any) error {
	var bodyReader io.Reader
	if bodyIn != nil {
		b, err := json.Marshal(bodyIn)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gateway error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	if respOut != nil {
		if err := json.NewDecoder(resp.Body).Decode(respOut); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *ClientBackend) SubmitTask(ctx context.Context, nodeID, capability string, input []byte, timeoutSec int) (protocol.Task, error) {
	req := protocol.SubmitRequest{
		NodeID:            nodeID,
		Capability:        capability,
		CapabilityVersion: 1,
		Input:             input,
		TimeoutSeconds:    timeoutSec,
	}
	var task protocol.Task
	err := c.do(ctx, "POST", "/v1/operator/tasks/submit", req, &task)
	return task, err
}

func (c *ClientBackend) GetTask(ctx context.Context, taskID string) (protocol.Task, error) {
	var task protocol.Task
	err := c.do(ctx, "GET", "/v1/operator/tasks/"+taskID, nil, &task)
	return task, err
}

func (c *ClientBackend) CancelTask(ctx context.Context, taskID string) (protocol.Task, error) {
	var task protocol.Task
	err := c.do(ctx, "POST", "/v1/operator/tasks/"+taskID+"/cancel", nil, &task)
	return task, err
}

func (c *ClientBackend) ListDevices(ctx context.Context) ([]devicestore.Device, error) {
	var devices []devicestore.Device
	err := c.do(ctx, "GET", "/v1/operator/devices", nil, &devices)
	return devices, err
}

func (c *ClientBackend) Diagnose(ctx context.Context) (doctor.Report, error) {
	var rep doctor.Report
	err := c.do(ctx, "GET", "/v1/doctor", nil, &rep)
	return rep, err
}

// OpenLocalBackend initializes a LocalBackend by opening databases in dataDir.
func OpenLocalBackend(dataDir string) (*LocalBackend, func(), error) {
	tasksPath := filepath.Join(dataDir, "tasks.sqlite")
	devsPath := filepath.Join(dataDir, "devices.sqlite")

	tasks, err := taskstore.Open(tasksPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open tasks store: %w", err)
	}

	devs, err := devicestore.Open(devsPath)
	if err != nil {
		_ = tasks.Close()
		return nil, nil, fmt.Errorf("open devices store: %w", err)
	}

	cleanup := func() {
		_ = tasks.Close()
		_ = devs.Close()
	}

	return &LocalBackend{
		Tasks:   tasks,
		Devices: devs,
		DataDir: dataDir,
	}, cleanup, nil
}
