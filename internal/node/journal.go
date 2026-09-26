package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Journal states, in the order one attempt normally passes through them.
const (
	// JournalClaimed is written before the gateway is told the node took the
	// lease: the node must be able to prove later that it saw the work.
	JournalClaimed = "claimed"
	// JournalStarting is written after the start was confirmed and before the
	// process is launched. It is the last state before an external side effect
	// can happen, which is why recovery treats it as "must not re-run".
	JournalStarting       = "starting"
	JournalResultPending  = "result_pending"
	JournalAcknowledged   = "acknowledged"
	JournalLeaseLost      = "lease_lost"
	JournalAborted        = "aborted"
	JournalRejected       = "rejected"
	JournalNeedsReconcile = "needs_reconciliation"
)

// JournalEntry is one state transition of one attempt.
type JournalEntry struct {
	Time      time.Time `json:"time"`
	TaskID    string    `json:"task_id"`
	AttemptID string    `json:"attempt_id"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
}

// finished reports whether an attempt reached a state that will not be
// resumed. Anything else is an attempt a crash may have interrupted.
func (e JournalEntry) finished() bool {
	switch e.State {
	case JournalAcknowledged, JournalLeaseLost, JournalAborted, JournalRejected, JournalNeedsReconcile:
		return true
	default:
		return false
	}
}

// attemptKey identifies one execution of one task.
type attemptKey struct {
	TaskID    string
	AttemptID string
}

// Journal is the node's local record of what it did. Every entry is flushed
// before the next side effect is allowed to happen, so a crash never leaves the
// node unable to say whether it had already taken responsibility for a task.
type Journal struct {
	mu   sync.Mutex
	path string
	file *os.File
}

// OpenJournal opens (creating if needed) the journal at path.
func OpenJournal(path string) (*Journal, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	return &Journal{path: path, file: file}, nil
}

// Append records one transition and flushes it to disk.
func (j *Journal) Append(entry JournalEntry) error {
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode journal entry: %w", err)
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return fmt.Errorf("journal %s is closed", j.path)
	}
	if _, err := j.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append journal: %w", err)
	}
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("flush journal: %w", err)
	}
	return nil
}

// Close releases the journal file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

// Entries reads the journal from disk, tolerating a torn final line: a crash
// mid-write must not make the whole history unreadable.
func (j *Journal) Entries() ([]JournalEntry, error) {
	file, err := os.Open(j.path)
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	defer file.Close()

	var entries []JournalEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry JournalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			// A partial trailing line is the expected shape of a crash during
			// an append; anything else is corruption worth surfacing.
			break
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("scan journal: %w", err)
	}
	return entries, nil
}

// Unfinished returns the attempts whose last recorded state says the node may
// have been interrupted while it held responsibility for the work.
func Unfinished(entries []JournalEntry) []JournalEntry {
	last := make(map[attemptKey]JournalEntry)
	order := make([]attemptKey, 0, len(entries))
	for _, entry := range entries {
		key := attemptKey{TaskID: entry.TaskID, AttemptID: entry.AttemptID}
		if _, seen := last[key]; !seen {
			order = append(order, key)
		}
		last[key] = entry
	}

	var unfinished []JournalEntry
	for _, key := range order {
		if entry := last[key]; !entry.finished() {
			unfinished = append(unfinished, entry)
		}
	}
	return unfinished
}
