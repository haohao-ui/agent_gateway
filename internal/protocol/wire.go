package protocol

import (
	"encoding/json"
	"time"
)

const (
	// CurrentProtocolVersion represents the wire envelope protocol version.
	CurrentProtocolVersion = 1

	// Event Types for real-time streaming
	EventTypeStdout        = "stdout"
	EventTypeStderr        = "stderr"
	EventTypeStatus        = "status"
	EventTypeInputRequest  = "input_request"
	EventTypeInputResponse = "input_response"
)

// NodeSoftwareVersion is the current build version of mesh node executable.
var NodeSoftwareVersion = FullVersion()

// PairInvitation carries the one-time secret and metadata displayed to the admin.
type PairInvitation struct {
	Token             string    `json:"token"`
	ExpiresAt         time.Time `json:"expires_at"`
	ServerFingerprint string    `json:"server_fingerprint"`
	MaxUses           int       `json:"max_uses,omitempty"`
	UseCount          int       `json:"use_count,omitempty"`
}

// PairRequest is sent by a node during unauthenticated bootstrap over server TLS.
type PairRequest struct {
	InvitationToken string `json:"invitation_token"`
	// CSRPEM is the Certificate Signing Request generated locally by the node.
	CSRPEM    string `json:"csr_pem"`
	MachineID string `json:"machine_id,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
}

// PairResponse returns the signed node certificate and CA certificate.
type PairResponse struct {
	NodeID        string `json:"node_id"`
	CertPEM       string `json:"cert_pem"`
	CACertPEM     string `json:"ca_cert_pem"`
	ServerVersion int    `json:"server_version"`
}

// AgentSoftware describes a detected AI application, runtime, or CLI tool on the node host.
type AgentSoftware struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // "cli" or "gui"
	Runnable bool   `json:"runnable"`
	Path     string `json:"path,omitempty"`
}

// ClaimRequest is sent by a node over mTLS to pull a queued task.
type ClaimRequest struct {
	LeaseDurationSeconds int             `json:"lease_duration_seconds"`
	NodeVersion          string          `json:"node_version,omitempty"`
	OS                   string          `json:"os,omitempty"`
	Arch                 string          `json:"arch,omitempty"`
	Hostname             string          `json:"hostname,omitempty"`
	MachineID            string          `json:"machine_id,omitempty"`
	Agents               []AgentSoftware `json:"agents,omitempty"`
	StartedAt            int64           `json:"started_at,omitempty"`
}

// RenewRequest extends a task lease while execution is ongoing.
type RenewRequest struct {
	TaskID               string `json:"task_id"`
	AttemptID            string `json:"attempt_id"`
	Token                string `json:"token"`
	LeaseDurationSeconds int    `json:"lease_duration_seconds"`
}

// RenewResponse confirms the extended lease deadline.
type RenewResponse struct {
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// CompleteRequest submits final execution output with the attempt token.
type CompleteRequest struct {
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`
	Token     string `json:"token"`
	Result    Result `json:"result"`
}

// TaskEvent represents one discrete streaming chunk or interactive event.
// Events form an ordered, monotonically increasing sequence per task attempt.
type TaskEvent struct {
	TaskID    string          `json:"task_id"`
	AttemptID string          `json:"attempt_id"`
	Sequence  int64           `json:"sequence"`
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
}

// ErrorResponse is the standard HTTP error payload for non-2xx responses.
type ErrorResponse struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}
