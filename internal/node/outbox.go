package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agent-gateway/internal/protocol"
)

// OutboxEntry is a finished result that has not been acknowledged yet. It
// carries the attempt credential because reporting it later is exactly the
// point: a lost acknowledgement must be recoverable after a restart, and the
// gateway accepts a result only for the attempt that holds the lease.
//
// The file lives in the node's private directory with mode 0600, next to the
// node key, and its token is never logged.
type OutboxEntry struct {
	TaskID    string          `json:"task_id"`
	AttemptID string          `json:"attempt_id"`
	Token     string          `json:"token"`
	Result    protocol.Result `json:"result"`
	WrittenAt time.Time       `json:"written_at"`
	// Reports counts how many delivery attempts have been made, so a repeated
	// failure is visible instead of silent.
	Reports int `json:"reports"`
}

func (e OutboxEntry) key() attemptKey {
	return attemptKey{TaskID: e.TaskID, AttemptID: e.AttemptID}
}

// Outbox stores pending results on disk before they are reported.
type Outbox struct {
	dir     string
	rejectD string
}

// OpenOutbox prepares the outbox directory.
func OpenOutbox(dir string) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create outbox: %w", err)
	}
	rejectD := filepath.Join(dir, "rejected")
	if err := os.MkdirAll(rejectD, 0o700); err != nil {
		return nil, fmt.Errorf("create outbox reject directory: %w", err)
	}
	return &Outbox{dir: dir, rejectD: rejectD}, nil
}

// Put writes a result to the outbox, replacing any earlier copy for the same
// attempt: the newest reported state is the one worth keeping.
func (o *Outbox) Put(entry OutboxEntry) error {
	path, err := o.path(entry)
	if err != nil {
		return err
	}
	if entry.WrittenAt.IsZero() {
		entry.WrittenAt = time.Now().UTC()
	}
	encoded, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("encode outbox entry: %w", err)
	}
	return writeFile(path, append(encoded, '\n'))
}

// List returns every pending entry, oldest first.
func (o *Outbox) List() ([]OutboxEntry, error) {
	dirents, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}

	var entries []OutboxEntry
	for _, dirent := range dirents {
		if dirent.IsDir() || filepath.Ext(dirent.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(o.dir, dirent.Name()))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read outbox entry %s: %w", dirent.Name(), err)
		}
		var entry OutboxEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("parse outbox entry %s: %w", dirent.Name(), err)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].WrittenAt.Before(entries[j].WrittenAt) })
	return entries, nil
}

// Remove drops a delivered entry.
func (o *Outbox) Remove(entry OutboxEntry) error {
	path, err := o.path(entry)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove outbox entry: %w", err)
	}
	return nil
}

// Reject moves an entry the gateway will never accept out of the way, keeping
// the bytes for an operator instead of deleting the only evidence.
func (o *Outbox) Reject(entry OutboxEntry, reason string) error {
	path, err := o.path(entry)
	if err != nil {
		return err
	}
	target := filepath.Join(o.rejectD, filepath.Base(path))
	if err := os.Rename(path, target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("move rejected outbox entry: %w", err)
	}
	return writeFile(target+".reason", []byte(reason+"\n"))
}

// path builds the entry file name, refusing identifiers that could escape the
// outbox directory. Task and attempt IDs are server generated, so a strange one
// means something is wrong upstream.
func (o *Outbox) path(entry OutboxEntry) (string, error) {
	for _, id := range []string{entry.TaskID, entry.AttemptID} {
		if id == "" || strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
			return "", fmt.Errorf("%w: unsafe identifier %q", protocol.ErrInvalid, id)
		}
	}
	return filepath.Join(o.dir, entry.TaskID+"."+entry.AttemptID+".json"), nil
}
