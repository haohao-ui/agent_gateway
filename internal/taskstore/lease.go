package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agent-gateway/internal/protocol"
)

// attemptExpired is the attempt state recorded when a lease expires. Every
// other attempt state reuses the protocol.State vocabulary.
const attemptExpired = "expired"

// Claim leases the oldest queued task for a node and returns it with a fresh
// secret token.
//
// The token is returned exactly once, here. Only its hash is stored, so a
// stolen database or a leaked task listing cannot be used to complete a task.
// When the node has no queued work Claim returns (nil, nil): an empty queue is
// not an error, and the caller is expected to poll again.
//
// The attempt ID and the token are generated server side; a caller cannot
// choose either. The claim takes the write lock, so two nodes can never be
// handed the same task.
func (s *Store) Claim(ctx context.Context, nodeID string, leaseFor time.Duration) (*protocol.Lease, error) {
	if err := validateLabel("node_id", nodeID, maxLabelBytes); err != nil {
		return nil, err
	}
	if err := validateLease(leaseFor); err != nil {
		return nil, err
	}
	// Probe before taking the write lock. A node polls for work far more often
	// than it finds any, and paying for a write transaction per idle poll would
	// serialize every idle node behind the writer for nothing.
	var queued string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM tasks WHERE node_id = ? AND state = 'queued' ORDER BY seq LIMIT 1`,
		nodeID).Scan(&queued)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, dbError("find queued task", err)
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}
	attemptID, err := newID(attemptIDPrefix)
	if err != nil {
		return nil, err
	}
	hash := tokenHash(token)

	var lease *protocol.Lease
	err = s.writeTx(ctx, func(q querier, now time.Time) error {
		// The lease runs from the moment the write lock was acquired, not from
		// the moment the caller asked: a claim that waited behind another
		// writer still hands out the full leaseFor.
		expires := now.Add(leaseFor)
		// Authoritative read: under the write lock, this row is the task this
		// claim owns.
		var taskID string
		err := q.QueryRowContext(ctx,
			`SELECT id FROM tasks WHERE node_id = ? AND state = 'queued' ORDER BY seq LIMIT 1`,
			nodeID).Scan(&taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return dbError("find queued task", err)
		}
		res, err := q.ExecContext(ctx,
			`UPDATE tasks SET state = 'leased', attempt_id = ?, lease_expires_at = ?, updated_at = ?
			 WHERE id = ? AND state = 'queued'`,
			attemptID, formatTime(expires), formatTime(now), taskID)
		if err != nil {
			return dbError("lease task", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return dbError("lease task", err)
		}
		if affected != 1 {
			// Unreachable while the write lock is held; kept as a guard so a
			// future change to the transaction shape cannot silently produce a
			// lease without a state change.
			return conflictf("task %s changed state while being claimed", taskID)
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO attempts (id, task_id, node_id, token_hash, state, lease_expires_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'leased', ?, ?, ?)`,
			attemptID, taskID, nodeID, hash, formatTime(expires), formatTime(now), formatTime(now)); err != nil {
			return dbError("record attempt", err)
		}
		t, err := readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		lease = &protocol.Lease{Task: t, Token: token}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lease, nil
}

// Start moves a leased task to running. The node calls it once it has recorded
// the lease locally and is about to launch the work, which is why a task that
// was never started cannot be completed successfully: the gateway has no
// evidence the node took responsibility for it.
func (s *Store) Start(ctx context.Context, taskID, attemptID, token string) error {
	if err := validateCredentialArgs(taskID, attemptID, token); err != nil {
		return err
	}
	return s.writeTx(ctx, func(q querier, now time.Time) error {
		t, err := readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		if err := checkAttemptCredentials(ctx, q, t, attemptID, token); err != nil {
			return err
		}
		if t.State != protocol.Leased {
			return conflictf("task %s is %s, not leased", t.ID, string(t.State))
		}
		// now is the instant the write lock was taken, so a lease that lapsed
		// while this call waited for the lock is rejected rather than revived.
		if err := checkLeaseActive(t, now); err != nil {
			return err
		}
		if err := updateTaskState(ctx, q, t.ID, protocol.Leased, protocol.Running, now); err != nil {
			return err
		}
		return updateAttemptState(ctx, q, attemptID, string(protocol.Running), now)
	})
}

// Renew extends the lease of a task that is leased, running or waiting for its
// cancellation to be reported. Renewing an expired lease fails: once a lease
// has lapsed the execution is no longer fenced, and the task must go to
// unknown through Expire instead of being quietly extended.
func (s *Store) Renew(ctx context.Context, taskID, attemptID, token string, leaseFor time.Duration) error {
	if err := validateCredentialArgs(taskID, attemptID, token); err != nil {
		return err
	}
	if err := validateLease(leaseFor); err != nil {
		return err
	}
	return s.writeTx(ctx, func(q querier, now time.Time) error {
		// The extension is measured from the write lock, like the claim that
		// started the lease.
		expires := now.Add(leaseFor)
		t, err := readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		if err := checkAttemptCredentials(ctx, q, t, attemptID, token); err != nil {
			return err
		}
		switch t.State {
		case protocol.Leased, protocol.Running, protocol.CancelRequested:
		default:
			return conflictf("task %s is %s and cannot be renewed", t.ID, string(t.State))
		}
		if err := checkLeaseActive(t, now); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE tasks SET lease_expires_at = ?, updated_at = ? WHERE id = ?`,
			formatTime(expires), formatTime(now), t.ID); err != nil {
			return dbError("renew task lease", err)
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE attempts SET lease_expires_at = ?, updated_at = ? WHERE id = ?`,
			formatTime(expires), formatTime(now), attemptID); err != nil {
			return dbError("renew attempt lease", err)
		}
		return nil
	})
}

// Complete records the terminal outcome of a task.
//
// A running task accepts succeeded, failed, or cancelled (a node that gave up
// locally reports cancelled). A task whose cancellation was requested accepts
// only cancelled: the cancellation arrived first, so a success report is a
// conflict rather than a silent overwrite. A leased task cannot be completed
// at all — Start must come first.
//
// The call is idempotent for the caller that recorded the result: retransmit
// the same result with the same attempt credentials and it succeeds, which is
// how a node recovers from a lost acknowledgement. A different result for the
// same attempt is a conflict, and a task that expired into unknown cannot be
// completed at all.
func (s *Store) Complete(ctx context.Context, taskID, attemptID, token string, result protocol.Result) error {
	if err := validateCredentialArgs(taskID, attemptID, token); err != nil {
		return err
	}
	if err := validateResult(result); err != nil {
		return err
	}
	return s.writeTx(ctx, func(q querier, now time.Time) error {
		t, err := readTask(ctx, q, taskID)
		if err != nil {
			return err
		}
		if err := checkAttemptCredentials(ctx, q, t, attemptID, token); err != nil {
			return err
		}
		switch t.State {
		case protocol.Succeeded, protocol.Failed, protocol.Cancelled:
			// Retransmission after a lost acknowledgement. The lease may have
			// expired since — including while this call waited for the write
			// lock — but the recorded credential plus an identical result are
			// what make the retransmission safe, so expiry is deliberately not
			// re-checked on this path.
			if t.Result == nil {
				return conflictf("task %s is %s without a recorded result", t.ID, string(t.State))
			}
			if !sameResult(*t.Result, result) {
				return conflictf("task %s already recorded a different result", t.ID)
			}
			return nil
		case protocol.Leased:
			return conflictf("task %s is leased; start it before completing it", t.ID)
		}
		// A task parked in unknown state accepts a completion from the node that
		// holds the current attempt credentials without requiring an active lease.
		if t.State != protocol.Unknown {
			if err := checkLeaseActive(t, now); err != nil {
				return err
			}
		}
		var next protocol.State
		switch t.State {
		case protocol.Running, protocol.Unknown:
			next = result.State
		case protocol.CancelRequested:
			if result.State != protocol.Cancelled {
				return conflictf("cancellation of task %s was requested first", t.ID)
			}
			next = protocol.Cancelled
		default:
			return conflictf("task %s is %s and cannot be completed", t.ID, string(t.State))
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE tasks SET state = ?, lease_expires_at = NULL, result_state = ?, result_text = ?,
			 result_exit_code = ?, result_error_code = ?, result_truncated = ?, updated_at = ?
			 WHERE id = ? AND state = ?`,
			string(next), string(result.State), result.Text, result.ExitCode, result.ErrorCode,
			boolToInt(result.Truncated), formatTime(now), t.ID, string(t.State)); err != nil {
			return dbError("complete task", err)
		}
		return updateAttemptState(ctx, q, attemptID, string(next), now)
	})
}

// Expire moves every task whose lease ended at or before now to unknown, and
// returns how many tasks it moved.
//
// unknown is deliberate rather than a requeue: once a lease has lapsed the
// gateway no longer knows whether the work ran, and re-running it could repeat
// a side effect. Reconciliation is an explicit operator action in a later
// milestone. Attempts keep their row, so an unknown task still points at the
// execution that has to be checked.
//
// now is a parameter so a caller (and a test) can expire leases deterministically
// without sleeping. Expire is idempotent: a second call with the same now
// returns 0.
//
// The supplied instant is the authority for which leases count as lapsed, so
// unlike the other mutations this one does not use the store clock: a sweep
// asked for "as of T" must not expire leases that ended after T just because
// the sweep had to wait for the write lock.
func (s *Store) Expire(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		return 0, invalidf("expire time must not be zero")
	}
	cutoff := formatTime(now)
	stamp := formatTime(now)
	var expired int64
	err := s.writeTx(ctx, func(q querier, _ time.Time) error {
		ids, err := expiredTaskIDs(ctx, q, cutoff)
		if err != nil {
			return err
		}
		for _, id := range ids {
			res, err := q.ExecContext(ctx,
				`UPDATE tasks SET state = 'unknown', lease_expires_at = NULL, updated_at = ?
				 WHERE id = ? AND state IN ('leased','running','cancel_requested') AND lease_expires_at <= ?`,
				stamp, id, cutoff)
			if err != nil {
				return dbError("expire task", err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return dbError("expire task", err)
			}
			if affected == 0 {
				continue
			}
			expired += affected
			if _, err := q.ExecContext(ctx,
				`UPDATE attempts SET state = ?, updated_at = ? WHERE task_id = ?`,
				attemptExpired, stamp, id); err != nil {
				return dbError("expire attempt", err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return expired, nil
}

// expiredTaskIDs lists the tasks whose lease has lapsed. The comparison is on
// the fixed-width UTC text written by formatTime, which orders the same way the
// timestamps do.
func expiredTaskIDs(ctx context.Context, q querier, cutoff string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id FROM tasks
		 WHERE state IN ('leased','running','cancel_requested')
		   AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?
		 ORDER BY seq`, cutoff)
	if err != nil {
		return nil, dbError("find expired leases", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, dbError("read expired lease", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("find expired leases", err)
	}
	return ids, nil
}

// checkAttemptCredentials verifies that a mutation is presented with the
// credentials of an attempt of this task, and that the attempt is the one the
// task is currently fenced to.
//
// Credentials are checked before fencing, so an attempt that does not exist
// and a token that does not match produce the same kind of error: a caller
// cannot use the error to learn which attempt IDs exist. The token itself is
// never part of the message.
//
// The fencing branch is unreachable in M1, where a task has exactly one
// attempt that is never superseded. It is kept for the re-lease path a later
// milestone adds, where a genuine credential for an old attempt must not be
// able to write to the task again.
func checkAttemptCredentials(ctx context.Context, q querier, t protocol.Task, attemptID, token string) error {
	var hash string
	err := q.QueryRowContext(ctx, `SELECT token_hash FROM attempts WHERE id = ? AND task_id = ?`, attemptID, t.ID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return unauthorizedf("unknown attempt for task %s", t.ID)
	}
	if err != nil {
		return dbError("read attempt", err)
	}
	if !tokenMatches(hash, token) {
		return unauthorizedf("invalid lease token for task %s", t.ID)
	}
	if t.AttemptID != attemptID {
		return conflictf("attempt %s is not the current attempt of task %s", attemptID, t.ID)
	}
	return nil
}

// checkLeaseActive rejects a mutation against a lease that has ended. It is
// the second half of the credential check: a correct token stops being
// sufficient once the lease lapses, which is what fences a node whose network
// or process stalled.
func checkLeaseActive(t protocol.Task, now time.Time) error {
	if t.LeaseExpiresAt == nil {
		return expiredf("task %s has no active lease", t.ID)
	}
	if !t.LeaseExpiresAt.After(now) {
		return expiredf("lease of task %s expired at %s", t.ID, formatTime(*t.LeaseExpiresAt))
	}
	return nil
}

// updateTaskState applies a guarded transition: the row must still be in the
// state the caller read. The guard is redundant under the write lock and is
// kept so a future change to the transaction shape fails loudly instead of
// silently overwriting a state the caller never saw.
func updateTaskState(ctx context.Context, q querier, taskID string, from, to protocol.State, now time.Time) error {
	res, err := q.ExecContext(ctx,
		`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		string(to), formatTime(now), taskID, string(from))
	if err != nil {
		return dbError("update task state", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return dbError("update task state", err)
	}
	if affected != 1 {
		return conflictf("task %s is no longer %s", taskID, string(from))
	}
	return nil
}

func updateAttemptState(ctx context.Context, q querier, attemptID, state string, now time.Time) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE attempts SET state = ?, updated_at = ? WHERE id = ?`,
		state, formatTime(now), attemptID); err != nil {
		return dbError("update attempt state", err)
	}
	return nil
}

// sameResult compares every field: a retransmission must reproduce the result
// exactly, so a node cannot amend an outcome after the fact.
func sameResult(recorded, incoming protocol.Result) bool {
	return recorded.State == incoming.State &&
		recorded.Text == incoming.Text &&
		recorded.ExitCode == incoming.ExitCode &&
		recorded.ErrorCode == incoming.ErrorCode &&
		recorded.Truncated == incoming.Truncated
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
