package taskstore

import (
	"context"
	"time"

	"agent-gateway/internal/protocol"
)

// Requeue resets a task in unknown state back to queued so a healthy node can claim it again.
// The attempt pointer is cleared and a new attempt will be generated upon subsequent claim.
func (s *Store) Requeue(ctx context.Context, taskID string) (protocol.Task, error) {
	if err := validateLabel("task_id", taskID, maxIDBytes); err != nil {
		return protocol.Task{}, err
	}
	var current protocol.Task
	err := s.writeTx(ctx, func(q querier, now time.Time) error {
		t, err := readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		if t.State != protocol.Unknown {
			return conflictf("task %s is %s; only unknown tasks can be requeued", t.ID, string(t.State))
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE tasks SET state = 'queued', attempt_id = NULL, lease_expires_at = NULL, updated_at = ?
			 WHERE id = ? AND state = 'unknown'`,
			formatTime(now), taskID); err != nil {
			return dbError("requeue task", err)
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

// Resolve allows an operator to definitively resolve a task parked in unknown state,
// setting its terminal state (failed or cancelled) and recording the operator's rationale.
func (s *Store) Resolve(ctx context.Context, taskID string, result protocol.Result) (protocol.Task, error) {
	if err := validateLabel("task_id", taskID, maxIDBytes); err != nil {
		return protocol.Task{}, err
	}
	if err := validateResult(result); err != nil {
		return protocol.Task{}, err
	}
	if result.State != protocol.Failed && result.State != protocol.Cancelled {
		return protocol.Task{}, conflictf("resolve outcome must be failed or cancelled, got %s", string(result.State))
	}
	var current protocol.Task
	err := s.writeTx(ctx, func(q querier, now time.Time) error {
		t, err := readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		if t.State != protocol.Unknown {
			return conflictf("task %s is %s; only unknown tasks can be resolved", t.ID, string(t.State))
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE tasks SET state = ?, lease_expires_at = NULL, result_state = ?, result_text = ?,
			 result_exit_code = ?, result_error_code = ?, result_truncated = ?, updated_at = ?
			 WHERE id = ? AND state = 'unknown'`,
			string(result.State), string(result.State), result.Text, result.ExitCode, result.ErrorCode,
			boolToInt(result.Truncated), formatTime(now), taskID); err != nil {
			return dbError("resolve task", err)
		}
		if t.AttemptID != "" {
			_ = updateAttemptState(ctx, q, t.AttemptID, string(result.State), now)
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

// ListUnknown returns tasks currently parked in unknown state up to limit entries.
func (s *Store) ListUnknown(ctx context.Context, limit int) ([]protocol.Task, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, node_id, capability, capability_version, input, timeout_seconds,
		 state, attempt_id, lease_expires_at, result_state, result_text,
		 result_exit_code, result_error_code, result_truncated, created_at, updated_at
		 FROM tasks WHERE state = 'unknown' ORDER BY updated_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, dbError("list unknown tasks", err)
	}
	defer rows.Close()

	var tasks []protocol.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, dbError("scan unknown task", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("list unknown tasks", err)
	}
	return tasks, nil
}
