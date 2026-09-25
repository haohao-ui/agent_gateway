package taskstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agent-gateway/internal/protocol"
)

// taskColumns is the column list every task read uses, in the order taskRow
// scans them. One shared list keeps the queries and the scanner from drifting
// apart.
const taskColumns = `id, node_id, capability, capability_version, input, timeout_seconds, state,
	attempt_id, lease_expires_at, result_state, result_text, result_exit_code, result_error_code,
	result_truncated, created_at, updated_at`

// Submit records a new task, or returns the task a previous submission with
// the same (node, idempotency key) already created.
//
// An empty idempotency key always creates a new task. With a key, the payload
// is compared to the one the key was first used with: a payload that is
// byte-identical after whitespace compaction returns the original task, any
// other payload returns protocol.ErrConflict. Identity is lexical, not
// semantic: reordered object keys and equivalent JSON escape spellings count as
// different payloads, and are rejected rather than merged.
//
// The lookup, the insert and the idempotency record share one write
// transaction, so two concurrent submissions of the same key cannot both
// create a task.
func (s *Store) Submit(ctx context.Context, in protocol.SubmitRequest) (protocol.Task, error) {
	payload, err := validateSubmit(in)
	if err != nil {
		return protocol.Task{}, err
	}
	var submitted protocol.Task
	err = s.writeTx(ctx, func(q querier, now time.Time) error {
		if in.IdempotencyKey != "" {
			taskID, hash, err := lookupIdempotency(ctx, q, in.NodeID, in.IdempotencyKey)
			if err != nil {
				return err
			}
			if taskID != "" {
				if hash != payload.hash {
					return conflictf("idempotency key was already used with a different payload")
				}
				t, err := readTask(ctx, q, taskID)
				if err != nil {
					return err
				}
				submitted = t
				return nil
			}
		}
		taskID, err := newID(taskIDPrefix)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO tasks (id, node_id, capability, capability_version, input, timeout_seconds, state, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
			taskID, in.NodeID, in.Capability, in.CapabilityVersion, string(payload.input),
			in.TimeoutSeconds, formatTime(now), formatTime(now)); err != nil {
			return dbError("insert task", err)
		}
		if in.IdempotencyKey != "" {
			if _, err := q.ExecContext(ctx,
				`INSERT INTO idempotency (node_id, idem_key, task_id, payload_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
				in.NodeID, in.IdempotencyKey, taskID, payload.hash, formatTime(now)); err != nil {
				return dbError("record idempotency key", err)
			}
		}
		t, err := readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		submitted = t
		return nil
	})
	if err != nil {
		return protocol.Task{}, err
	}
	return submitted, nil
}

// Get returns a task by ID. The returned value never carries a lease token:
// protocol.Task has no field for one.
func (s *Store) Get(ctx context.Context, taskID string) (protocol.Task, error) {
	if err := validateLabel("task_id", taskID, maxIDBytes); err != nil {
		return protocol.Task{}, err
	}
	return readTask(ctx, s.db, taskID)
}

// Cancel asks for a task to stop.
//
// A queued task becomes cancelled immediately: nothing has run, so there is no
// attempt and no result. A leased or running task becomes cancel_requested,
// which lets the node report back through Renew and Complete. Cancelling a
// task that is already cancel_requested or terminal is not an error; the
// current task is returned unchanged.
//
// Cancellation says nothing about side effects: a node that has already
// started a process must still stop it and report the outcome. Cancel takes no
// credential in M1 because the contract has no actor for it yet; M2 adds
// caller authorization.
func (s *Store) Cancel(ctx context.Context, taskID string) (protocol.Task, error) {
	if err := validateLabel("task_id", taskID, maxIDBytes); err != nil {
		return protocol.Task{}, err
	}
	var current protocol.Task
	err := s.writeTx(ctx, func(q querier, now time.Time) error {
		t, err := readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		var next protocol.State
		switch t.State {
		case protocol.Queued:
			next = protocol.Cancelled
		case protocol.Leased, protocol.Running:
			next = protocol.CancelRequested
		default:
			// cancel_requested, and every terminal state, already describe an
			// outcome: report it instead of moving the task again.
			current = t
			return nil
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
			string(next), formatTime(now), taskID); err != nil {
			return dbError("cancel task", err)
		}
		t, err = readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		current = t
		return nil
	})
	if err != nil {
		return protocol.Task{}, err
	}
	return current, nil
}

// lookupIdempotency returns the task recorded for a (node, key) pair together
// with the hash of the payload it was created with. An empty task ID means the
// key has not been used.
func lookupIdempotency(ctx context.Context, q querier, nodeID, key string) (taskID, hash string, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT task_id, payload_hash FROM idempotency WHERE node_id = ? AND idem_key = ?`,
		nodeID, key).Scan(&taskID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", dbError("look up idempotency key", err)
	}
	return taskID, hash, nil
}

// readTask loads one task, translating "no rows" into protocol.ErrNotFound so
// callers do not have to know about database/sql.
func readTask(ctx context.Context, q querier, taskID string) (protocol.Task, error) {
	t, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Task{}, notFoundf("task %s", taskID)
	}
	if err != nil {
		return protocol.Task{}, dbError("read task", err)
	}
	return t, nil
}

// scanner is the part of *sql.Row and *sql.Rows that scanTask needs.
type scanner interface {
	Scan(dest ...any) error
}

// taskRow mirrors one row of tasks. Nullable columns are the ones that only
// exist once a task is claimed or finished.
type taskRow struct {
	id                string
	nodeID            string
	capability        string
	capabilityVersion int
	input             string
	timeoutSeconds    int
	state             string
	attemptID         sql.NullString
	leaseExpiresAt    sql.NullString
	resultState       sql.NullString
	resultText        sql.NullString
	resultExitCode    sql.NullInt64
	resultErrorCode   sql.NullString
	resultTruncated   sql.NullInt64
	createdAt         string
	updatedAt         string
}

func scanTask(row scanner) (protocol.Task, error) {
	var r taskRow
	if err := row.Scan(&r.id, &r.nodeID, &r.capability, &r.capabilityVersion, &r.input, &r.timeoutSeconds,
		&r.state, &r.attemptID, &r.leaseExpiresAt, &r.resultState, &r.resultText, &r.resultExitCode,
		&r.resultErrorCode, &r.resultTruncated, &r.createdAt, &r.updatedAt); err != nil {
		return protocol.Task{}, err
	}
	return r.task()
}

// task converts a stored row into the shared type. A stored timestamp that
// does not parse is an internal error, not a caller error: only this package
// writes those columns.
func (r taskRow) task() (protocol.Task, error) {
	created, err := parseTime(r.createdAt)
	if err != nil {
		return protocol.Task{}, dbError("parse created_at", err)
	}
	updated, err := parseTime(r.updatedAt)
	if err != nil {
		return protocol.Task{}, dbError("parse updated_at", err)
	}
	t := protocol.Task{
		ID:                r.id,
		NodeID:            r.nodeID,
		Capability:        r.capability,
		CapabilityVersion: r.capabilityVersion,
		Input:             json.RawMessage(r.input),
		TimeoutSeconds:    r.timeoutSeconds,
		State:             protocol.State(r.state),
		CreatedAt:         created,
		UpdatedAt:         updated,
	}
	if r.attemptID.Valid {
		t.AttemptID = r.attemptID.String
	}
	if r.leaseExpiresAt.Valid {
		expires, err := parseTime(r.leaseExpiresAt.String)
		if err != nil {
			return protocol.Task{}, dbError("parse lease_expires_at", err)
		}
		t.LeaseExpiresAt = &expires
	}
	if r.resultState.Valid {
		t.Result = &protocol.Result{
			State:     protocol.State(r.resultState.String),
			Text:      r.resultText.String,
			ExitCode:  int(r.resultExitCode.Int64),
			ErrorCode: r.resultErrorCode.String,
			Truncated: r.resultTruncated.Int64 != 0,
		}
	}
	return t, nil
}
