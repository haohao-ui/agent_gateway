package taskstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"strconv"
	"time"
	"unicode/utf8"

	"agent-gateway/internal/protocol"
)

// Input limits from docs/CONTRACTS.md. Validation happens before any
// transaction opens, so an oversized or malformed request never reaches
// SQLite and never holds the write lock.
const (
	maxInputBytes    = 64 << 10 // capability input
	maxResultBytes   = 64 << 10 // Result.Text, measured in bytes
	maxLabelBytes    = 128      // node_id, capability
	maxIDBytes       = 128      // task_id, attempt_id as accepted on input
	maxIdemKeyBytes  = 256      // idempotency_key
	maxErrorCodeLen  = 64       // Result.ErrorCode
	maxTokenBytes    = 128      // opaque lease token, upper bound only
	minTimeoutSecond = 1
	maxTimeoutSecond = 3600
)

// maxLease is the longest lease a caller may request.
const maxLease = 5 * time.Minute

// submitPayload is a validated submission: the compacted input that will be
// stored, and the hash that identifies the payload for idempotency.
type submitPayload struct {
	input []byte
	hash  string
}

// validateSubmit checks a submission and returns its compacted form. The
// contract is explicit that capability is non-empty, version is at least 1,
// input is valid JSON of at most 64 KiB, and the timeout is 1..3600 seconds.
func validateSubmit(in protocol.SubmitRequest) (submitPayload, error) {
	if err := validateLabel("node_id", in.NodeID, maxLabelBytes); err != nil {
		return submitPayload{}, err
	}
	if err := validateLabel("capability", in.Capability, maxLabelBytes); err != nil {
		return submitPayload{}, err
	}
	if in.CapabilityVersion < 1 {
		return submitPayload{}, invalidf("capability_version must be at least 1, got %d", in.CapabilityVersion)
	}
	if in.TimeoutSeconds < minTimeoutSecond || in.TimeoutSeconds > maxTimeoutSecond {
		return submitPayload{}, invalidf("timeout_seconds must be between %d and %d, got %d",
			minTimeoutSecond, maxTimeoutSecond, in.TimeoutSeconds)
	}
	input, err := compactValidatedJSON("input", in.Input, maxInputBytes)
	if err != nil {
		return submitPayload{}, err
	}
	if in.IdempotencyKey != "" {
		if err := validateLabel("idempotency_key", in.IdempotencyKey, maxIdemKeyBytes); err != nil {
			return submitPayload{}, err
		}
	}
	return submitPayload{input: input, hash: payloadHash(in, input)}, nil
}

// validateResult checks the shape of a terminal result. The state is restricted
// to the three terminal states a node may report; the text is bounded and must
// be valid UTF-8 so that stored output cannot break readers that assume text.
func validateResult(res protocol.Result) error {
	switch res.State {
	case protocol.Succeeded, protocol.Failed, protocol.Cancelled:
	default:
		return invalidf("result state %q is not a terminal state", string(res.State))
	}
	if len(res.Text) > maxResultBytes {
		return invalidf("result text is %d bytes, limit is %d", len(res.Text), maxResultBytes)
	}
	if !utf8.ValidString(res.Text) {
		return invalidf("result text must be valid UTF-8")
	}
	if res.ErrorCode != "" {
		if len(res.ErrorCode) > maxErrorCodeLen {
			return invalidf("error_code is %d bytes, limit is %d", len(res.ErrorCode), maxErrorCodeLen)
		}
		for _, r := range res.ErrorCode {
			// Stable machine-readable categories only: no spaces, no prose.
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
				continue
			}
			return invalidf("error_code must match [a-z0-9_.-] and be at most %d bytes", maxErrorCodeLen)
		}
	}
	return nil
}

// validateLease bounds a requested lease duration. A zero or negative lease
// would create a task that is expired on arrival, and an unbounded one would
// keep an execution fenced off from recovery for as long as the node likes.
func validateLease(d time.Duration) error {
	if d <= 0 {
		return invalidf("lease duration must be positive")
	}
	if d > maxLease {
		return invalidf("lease duration %s exceeds the %s maximum", d, maxLease)
	}
	return nil
}

// validateCredentialArgs checks the (task_id, attempt_id, token) triple a
// mutation is authenticated with. The token is opaque: only its length is
// checked, and it is never echoed back in an error.
func validateCredentialArgs(taskID, attemptID, token string) error {
	if err := validateLabel("task_id", taskID, maxIDBytes); err != nil {
		return err
	}
	if err := validateLabel("attempt_id", attemptID, maxIDBytes); err != nil {
		return err
	}
	if token == "" {
		return invalidf("lease token must not be empty")
	}
	if len(token) > maxTokenBytes {
		return invalidf("lease token is longer than %d bytes", maxTokenBytes)
	}
	return nil
}

// validateLabel checks a caller-supplied identifier. Values are never echoed
// in the error: only the field name and its length are reported, so a bad
// request cannot reflect attacker-controlled bytes back into logs.
func validateLabel(field, value string, maxBytes int) error {
	if value == "" {
		return invalidf("%s must not be empty", field)
	}
	if len(value) > maxBytes {
		return invalidf("%s is %d bytes, limit is %d", field, len(value), maxBytes)
	}
	if !utf8.ValidString(value) {
		return invalidf("%s must be valid UTF-8", field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return invalidf("%s must not contain control characters", field)
		}
	}
	return nil
}

// compactValidatedJSON validates a JSON value and removes insignificant
// whitespace. It is the only normalisation this package performs.
//
// Payload identity is therefore lexical: after whitespace is stripped, two
// payloads are the same only if their bytes are the same. Object key order and
// JSON escape spelling are preserved, so {"a":1,"b":2} and {"b":2,"a":1}, or
// "中" and "中", are different payloads and an idempotent retry that spells
// a payload differently is rejected as a conflict rather than merged. That is
// deliberate for M1: it never merges two requests that were not written
// identically, and it needs no semantic JSON comparison.
func compactValidatedJSON(field string, raw []byte, maxBytes int) ([]byte, error) {
	if len(raw) == 0 {
		return nil, invalidf("%s must not be empty", field)
	}
	if len(raw) > maxBytes {
		return nil, invalidf("%s is %d bytes, limit is %d", field, len(raw), maxBytes)
	}
	if !utf8.Valid(raw) {
		return nil, invalidf("%s must be valid UTF-8", field)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, invalidf("%s must be valid JSON", field)
	}
	return buf.Bytes(), nil
}

// payloadHash identifies the submission a caller wants deduplicated. It covers
// the compacted input, so its notion of "the same payload" is the lexical one
// described on compactValidatedJSON. Fields are length-prefixed so that no two
// different field splits can produce the same digest.
func payloadHash(in protocol.SubmitRequest, compactedInput []byte) string {
	h := sha256.New()
	writeField(h, []byte(in.NodeID))
	writeField(h, []byte(in.Capability))
	writeField(h, []byte(strconv.Itoa(in.CapabilityVersion)))
	writeField(h, []byte(strconv.Itoa(in.TimeoutSeconds)))
	writeField(h, compactedInput)
	return hex.EncodeToString(h.Sum(nil))
}

func writeField(h hash.Hash, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}
