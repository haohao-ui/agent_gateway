package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func appendEntries(t *testing.T, journal *Journal, entries ...JournalEntry) {
	t.Helper()
	for _, entry := range entries {
		if err := journal.Append(entry); err != nil {
			t.Fatalf("append %s: %v", entry.State, err)
		}
	}
}

func TestJournalRoundTripAndUnfinished(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer journal.Close()

	appendEntries(t, journal,
		JournalEntry{TaskID: "task-1", AttemptID: "attempt-1", State: JournalClaimed},
		JournalEntry{TaskID: "task-1", AttemptID: "attempt-1", State: JournalStarting},
		JournalEntry{TaskID: "task-1", AttemptID: "attempt-1", State: JournalResultPending},
		JournalEntry{TaskID: "task-1", AttemptID: "attempt-1", State: JournalAcknowledged},
		JournalEntry{TaskID: "task-2", AttemptID: "attempt-2", State: JournalClaimed},
	)

	entries, err := journal.Entries()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("read %d entries, want 5", len(entries))
	}
	if entries[0].Time.IsZero() {
		t.Fatal("the journal did not stamp a time")
	}

	// Only the attempt that never reached a terminal state is unfinished; the
	// acknowledged one is not, even though its earlier entries are not terminal.
	unfinished := Unfinished(entries)
	if len(unfinished) != 1 {
		t.Fatalf("found %d unfinished attempts, want 1: %+v", len(unfinished), unfinished)
	}
	if unfinished[0].TaskID != "task-2" {
		t.Fatalf("the wrong attempt was reported as unfinished: %+v", unfinished[0])
	}
}

func TestJournalToleratesATornFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	appendEntries(t, journal, JournalEntry{TaskID: "task-1", AttemptID: "attempt-1", State: JournalClaimed})
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	// A crash in the middle of an append leaves half a line behind.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	if _, err := file.WriteString(`{"task_id":"task-2","attem`); err != nil {
		t.Fatalf("write partial line: %v", err)
	}
	file.Close()

	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	defer reopened.Close()

	entries, err := reopened.Entries()
	if err != nil {
		t.Fatalf("read journal with a torn line: %v", err)
	}
	if len(entries) != 1 || entries[0].TaskID != "task-1" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestOutboxLifecycle(t *testing.T) {
	outbox, err := OpenOutbox(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	entry := OutboxEntry{
		TaskID:    "task-1",
		AttemptID: "attempt-1",
		Token:     "secret-token",
		Result:    protocol.Result{State: protocol.Succeeded, Text: "done"},
	}

	if err := outbox.Put(entry); err != nil {
		t.Fatalf("put: %v", err)
	}
	entries, err := outbox.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("listed %d entries, want 1", len(entries))
	}
	// The token has to survive a restart: reporting later is the whole point.
	if entries[0].Token != "secret-token" || entries[0].Result.Text != "done" {
		t.Fatalf("entry did not round-trip: %+v", entries[0])
	}

	// A second copy for the same attempt replaces the first.
	entry.Reports = 3
	if err := outbox.Put(entry); err != nil {
		t.Fatalf("put again: %v", err)
	}
	entries, err = outbox.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Reports != 3 {
		t.Fatalf("replacement did not take: %+v", entries)
	}

	if err := outbox.Remove(entry); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if entries, err = outbox.List(); err != nil || len(entries) != 0 {
		t.Fatalf("outbox still holds %d entries (err %v)", len(entries), err)
	}
}

func TestOutboxRejectKeepsTheEvidence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outbox")
	outbox, err := OpenOutbox(dir)
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	entry := OutboxEntry{TaskID: "task-1", AttemptID: "attempt-1", Token: "t", Result: protocol.Result{State: protocol.Failed}}

	if err := outbox.Put(entry); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := outbox.Reject(entry, "gateway said no"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	if entries, _ := outbox.List(); len(entries) != 0 {
		t.Fatalf("a rejected entry is still pending: %+v", entries)
	}
	rejected, err := os.ReadDir(filepath.Join(dir, "rejected"))
	if err != nil {
		t.Fatalf("read rejected directory: %v", err)
	}
	if len(rejected) != 2 {
		t.Fatalf("expected the entry and its reason, found %d files", len(rejected))
	}
}

func TestOutboxRefusesUnsafeIdentifiers(t *testing.T) {
	outbox, err := OpenOutbox(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	for _, id := range []string{"../../escape", "a/b", `a\b`, "..", ""} {
		if err := outbox.Put(OutboxEntry{TaskID: id, AttemptID: "attempt-1"}); err == nil {
			t.Fatalf("identifier %q was accepted as a file name", id)
		}
	}
}

func TestOutboxOrdersOldestFirst(t *testing.T) {
	outbox, err := OpenOutbox(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	base := time.Now().UTC()
	for _, entry := range []OutboxEntry{
		{TaskID: "task-new", AttemptID: "attempt-1", WrittenAt: base.Add(time.Minute)},
		{TaskID: "task-old", AttemptID: "attempt-1", WrittenAt: base},
	} {
		if err := outbox.Put(entry); err != nil {
			t.Fatalf("put %s: %v", entry.TaskID, err)
		}
	}

	entries, err := outbox.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 || entries[0].TaskID != "task-old" {
		t.Fatalf("entries are not ordered oldest first: %+v", entries)
	}
}

func TestInstanceLockIsExclusiveAndReusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.lock")

	first, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := acquireInstanceLock(path); err == nil {
		t.Fatal("a second lock on the same file was granted")
	} else if !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("second lock returned %v, want a conflict", err)
	}

	if err := first(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := second(); err != nil {
		t.Fatalf("release second lock: %v", err)
	}
}
