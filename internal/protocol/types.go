// Package protocol defines versioned shared values, without transport or storage dependencies.
package protocol

import (
	"encoding/json"
	"errors"
	"time"
)

type State string

const (
	Queued          State = "queued"
	Leased          State = "leased"
	Running         State = "running"
	CancelRequested State = "cancel_requested"
	Succeeded       State = "succeeded"
	Failed          State = "failed"
	Cancelled       State = "cancelled"
	Unknown         State = "unknown"
)

var (
	ErrInvalid      = errors.New("invalid input")
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnauthorized = errors.New("unauthorized")
	ErrLeaseExpired = errors.New("lease expired")
)

// SubmitRequest is validated by the task service; identity authorization belongs to M2.
type SubmitRequest struct {
	NodeID            string          `json:"node_id"`
	Capability        string          `json:"capability"`
	CapabilityVersion int             `json:"capability_version"`
	Input             json.RawMessage `json:"input"`
	TimeoutSeconds    int             `json:"timeout_seconds"`
	IdempotencyKey    string          `json:"idempotency_key,omitempty"`
}

// Task is safe to return without exposing the attempt's credential.
type Task struct {
	ID                string          `json:"id"`
	NodeID            string          `json:"node_id"`
	Capability        string          `json:"capability"`
	CapabilityVersion int             `json:"capability_version"`
	Input             json.RawMessage `json:"input"`
	TimeoutSeconds    int             `json:"timeout_seconds"`
	State             State           `json:"state"`
	AttemptID         string          `json:"attempt_id,omitempty"`
	LeaseExpiresAt    *time.Time      `json:"lease_expires_at,omitempty"`
	Result            *Result         `json:"result,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// Lease contains a secret and must never be logged or included in task listings.
type Lease struct {
	Task  Task   `json:"task"`
	Token string `json:"token"`
}

// Result is a bounded final execution result; files will be separate artifact references.
type Result struct {
	State     State  `json:"state"`
	Text      string `json:"text"`
	ExitCode  int    `json:"exit_code"`
	ErrorCode string `json:"error_code,omitempty"`
	Truncated bool   `json:"truncated"`
}
