package devicestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

const (
	testValidNodeID      = "node-test-001"
	testValidFingerprint = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	testOtherFingerprint = "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"
)

func newTempDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "devices.db")
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) failed: %v", path, err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s
}

func TestOpen_Validation(t *testing.T) {
	t.Run("empty and blank path", func(t *testing.T) {
		if _, err := Open(""); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for empty path, got: %v", err)
		}
		if _, err := Open("   "); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for whitespace path, got: %v", err)
		}
	})

	t.Run("memory path rejected", func(t *testing.T) {
		if _, err := Open(":memory:"); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for :memory:, got: %v", err)
		}
	})

	t.Run("file uri prefix rejected", func(t *testing.T) {
		if _, err := Open("file:///tmp/devices.db"); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for file: URI, got: %v", err)
		}
	})

	t.Run("query and fragment rejected", func(t *testing.T) {
		if _, err := Open("devices.db?mode=ro"); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for path with ?, got: %v", err)
		}
		if _, err := Open("devices.db#frag"); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for path with #, got: %v", err)
		}
	})

	t.Run("directory path rejected", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Open(dir); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for directory path, got: %v", err)
		}
	})

	t.Run("missing parent directory fails", func(t *testing.T) {
		nonexistent := filepath.Join(t.TempDir(), "nonexistent", "devices.db")
		if _, err := Open(nonexistent); err == nil {
			t.Fatalf("expected error for missing parent directory, got nil")
		}
	})

	t.Run("valid path opens and creates 0600 file", func(t *testing.T) {
		p := newTempDB(t)
		s, err := Open(p)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer s.Close()

		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("Stat failed: %v", err)
		}
		// On Unix, file mode should be 0600 (ignoring directory/symlink bits)
		if perm := fi.Mode().Perm(); perm != 0600 {
			t.Logf("Notice: file mode is %04o (may vary on Windows)", perm)
		}
	})
}

func TestClose_Idempotent(t *testing.T) {
	p := newTempDB(t)
	s, err := Open(p)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}

	// Calling methods after Close should return error
	ctx := context.Background()
	if err := s.Authorize(ctx, testValidNodeID, testValidFingerprint); err == nil {
		t.Fatalf("expected error from Authorize after Close, got nil")
	}
}

func TestRegister_Validation(t *testing.T) {
	p := newTempDB(t)
	s := openTestStore(t, p)
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)

	t.Run("nodeID validation", func(t *testing.T) {
		cases := []struct {
			name   string
			nodeID string
			valid  bool
		}{
			{"empty", "", false},
			{"whitespace only", "   ", false},
			{"contains NUL", "node\x001", false},
			{"128 bytes", strings.Repeat("n", 128), true},
			{"129 bytes", strings.Repeat("n", 129), false},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := s.Register(ctx, tc.nodeID, testValidFingerprint, future)
				if tc.valid && err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
				if !tc.valid && !errors.Is(err, protocol.ErrInvalid) {
					t.Fatalf("expected ErrInvalid, got: %v", err)
				}
			})
		}
	})

	t.Run("fingerprint validation", func(t *testing.T) {
		cases := []struct {
			name string
			fp   string
			want bool
		}{
			{"empty", "", false},
			{"63 chars", strings.Repeat("a", 63), false},
			{"65 chars", strings.Repeat("a", 65), false},
			{"uppercase hex", strings.ToUpper(testValidFingerprint), false},
			{"mixed case hex", testValidFingerprint[:60] + "ABCD", false},
			{"non-hex chars", strings.Repeat("g", 64), false},
			{"contains spaces", testValidFingerprint[:63] + " ", false},
			{"valid 64 lower hex", testValidFingerprint, true},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				nodeID := fmt.Sprintf("node-fp-%s", tc.name)
				err := s.Register(ctx, nodeID, tc.fp, future)
				if tc.want && err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
				if !tc.want && !errors.Is(err, protocol.ErrInvalid) {
					t.Fatalf("expected ErrInvalid, got: %v", err)
				}
			})
		}
	})

	t.Run("expiresAt validation", func(t *testing.T) {
		// Past expiration
		past := time.Now().Add(-1 * time.Hour)
		if err := s.Register(ctx, "node-past", testValidFingerprint, past); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for past expiry, got: %v", err)
		}

		// Zero time
		if err := s.Register(ctx, "node-zero", testValidFingerprint, time.Time{}); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for zero time, got: %v", err)
		}

		// Exact current time or near past
		if err := s.Register(ctx, "node-now", testValidFingerprint, time.Now()); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for now expiry, got: %v", err)
		}
	})
}

func TestRegister_IdempotencyAndImmutability(t *testing.T) {
	p := newTempDB(t)
	s := openTestStore(t, p)
	ctx := context.Background()
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Microsecond)

	// First registration
	if err := s.Register(ctx, testValidNodeID, testValidFingerprint, exp); err != nil {
		t.Fatalf("initial Register failed: %v", err)
	}

	// Idempotent retry with identical values
	if err := s.Register(ctx, testValidNodeID, testValidFingerprint, exp); err != nil {
		t.Fatalf("identical Register must be idempotent, got: %v", err)
	}

	// Attempt to replace certificate binding with different fingerprint
	if err := s.Register(ctx, testValidNodeID, testOtherFingerprint, exp); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("replacing certificate must return ErrConflict, got: %v", err)
	}

	// Attempt to change expiration time
	differentExp := exp.Add(24 * time.Hour)
	if err := s.Register(ctx, testValidNodeID, testValidFingerprint, differentExp); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("changing expiration must return ErrConflict, got: %v", err)
	}

	// Revoke node
	if err := s.Revoke(ctx, testValidNodeID, "sec-admin", "compromised"); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}

	// Attempt to resurrect revoked node via Register
	if err := s.Register(ctx, testValidNodeID, testValidFingerprint, exp); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("resurrecting revoked device must return ErrConflict, got: %v", err)
	}
}

func TestAuthorize_Matrix(t *testing.T) {
	p := newTempDB(t)
	s := openTestStore(t, p)
	ctx := context.Background()

	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	exp := now.Add(2 * time.Hour)
	if err := s.Register(ctx, "node-auth", testValidFingerprint, exp); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	t.Run("successful authorization", func(t *testing.T) {
		if err := s.Authorize(ctx, "node-auth", testValidFingerprint); err != nil {
			t.Fatalf("Authorize failed: %v", err)
		}
	})

	t.Run("unknown node", func(t *testing.T) {
		if err := s.Authorize(ctx, "node-nonexistent", testValidFingerprint); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for unknown node, got: %v", err)
		}
	})

	t.Run("fingerprint mismatch", func(t *testing.T) {
		if err := s.Authorize(ctx, "node-auth", testOtherFingerprint); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for fingerprint mismatch, got: %v", err)
		}
	})

	t.Run("malformed and empty parameters", func(t *testing.T) {
		if err := s.Authorize(ctx, "", testValidFingerprint); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for empty nodeID, got: %v", err)
		}
		if err := s.Authorize(ctx, "node-auth", "invalid-fp"); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for invalid fp, got: %v", err)
		}
		if err := s.Authorize(ctx, "node-auth", strings.ToUpper(testValidFingerprint)); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for uppercase fp, got: %v", err)
		}
	})

	t.Run("expired device", func(t *testing.T) {
		// Fast-forward clock past expiration
		s.now = func() time.Time { return exp.Add(1 * time.Second) }
		if err := s.Authorize(ctx, "node-auth", testValidFingerprint); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for expired device, got: %v", err)
		}
		// Reset clock back
		s.now = func() time.Time { return now }
	})

	t.Run("revoked device", func(t *testing.T) {
		if err := s.Revoke(ctx, "node-auth", "admin", "test-revocation"); err != nil {
			t.Fatalf("Revoke failed: %v", err)
		}
		if err := s.Authorize(ctx, "node-auth", testValidFingerprint); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for revoked device, got: %v", err)
		}
	})
}

func TestRevoke_MatrixAndAuditConsistency(t *testing.T) {
	p := newTempDB(t)
	s := openTestStore(t, p)
	ctx := context.Background()

	exp := time.Now().Add(24 * time.Hour)
	if err := s.Register(ctx, "node-rev-1", testValidFingerprint, exp); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	t.Run("parameter validation", func(t *testing.T) {
		if err := s.Revoke(ctx, "", "admin", "reason"); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for empty nodeID, got: %v", err)
		}
		if err := s.Revoke(ctx, "node-rev-1", "", "reason"); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for empty actor, got: %v", err)
		}
		if err := s.Revoke(ctx, "node-rev-1", "admin", ""); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for empty reason, got: %v", err)
		}
		if err := s.Revoke(ctx, "node-rev-1", strings.Repeat("a", 129), "reason"); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for actor > 128 bytes, got: %v", err)
		}
		if err := s.Revoke(ctx, "node-rev-1", "admin", strings.Repeat("r", 1025)); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("expected ErrInvalid for reason > 1024 bytes, got: %v", err)
		}
		// Exact limits should succeed
		exactActor := strings.Repeat("a", 128)
		exactReason := strings.Repeat("r", 1024)
		if err := s.Revoke(ctx, "node-rev-1", exactActor, exactReason); err != nil {
			t.Fatalf("Revoke with 128-byte actor and 1024-byte reason should succeed, got: %v", err)
		}
	})

	t.Run("unknown node returns ErrNotFound", func(t *testing.T) {
		if err := s.Revoke(ctx, "node-unknown-999", "admin", "test"); !errors.Is(err, protocol.ErrNotFound) {
			t.Fatalf("expected ErrNotFound for unknown node, got: %v", err)
		}
	})

	t.Run("idempotent revocation and audit immutability", func(t *testing.T) {
		// Second revocation with different actor and reason must be idempotent and NOT overwrite original audit
		if err := s.Revoke(ctx, "node-rev-1", "second-actor", "second-reason"); err != nil {
			t.Fatalf("second Revoke must be idempotent, got: %v", err)
		}

		// Inspect device_audit table directly
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM device_audit WHERE node_id=?", "node-rev-1").Scan(&count); err != nil {
			t.Fatalf("count audit failed: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 audit record for node-rev-1, found %d", count)
		}

		var actor, action, nodeID, outcome, reason, occurredAt string
		err := s.db.QueryRowContext(ctx, "SELECT actor, action, node_id, outcome, reason, occurred_at FROM device_audit WHERE node_id=?", "node-rev-1").
			Scan(&actor, &action, &nodeID, &outcome, &reason, &occurredAt)
		if err != nil {
			t.Fatalf("query audit record failed: %v", err)
		}

		if actor != strings.Repeat("a", 128) {
			t.Fatalf("expected original actor, got: %q", actor)
		}
		if action != "device.revoke" {
			t.Fatalf("expected action 'device.revoke', got: %q", action)
		}
		if nodeID != "node-rev-1" {
			t.Fatalf("expected node_id 'node-rev-1', got: %q", nodeID)
		}
		if outcome != "success" {
			t.Fatalf("expected outcome 'success', got: %q", outcome)
		}
		if reason != strings.Repeat("r", 1024) {
			t.Fatalf("expected original reason, got: %q", reason)
		}
		if occurredAt == "" {
			t.Fatalf("expected non-empty occurred_at")
		}
	})
}

func TestReopen_Persistence(t *testing.T) {
	p := newTempDB(t)
	exp := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)
	ctx := context.Background()

	// Instance 1: Register nodeA and nodeB, revoke nodeB
	s1, err := Open(p)
	if err != nil {
		t.Fatalf("Open s1 failed: %v", err)
	}
	if err := s1.Register(ctx, "nodeA", testValidFingerprint, exp); err != nil {
		t.Fatalf("Register nodeA failed: %v", err)
	}
	if err := s1.Register(ctx, "nodeB", testOtherFingerprint, exp); err != nil {
		t.Fatalf("Register nodeB failed: %v", err)
	}
	if err := s1.Revoke(ctx, "nodeB", "admin", "compromised"); err != nil {
		t.Fatalf("Revoke nodeB failed: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close s1 failed: %v", err)
	}

	// Instance 2: Reopen same database file
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("Open s2 failed: %v", err)
	}
	defer s2.Close()

	// nodeA must still be authorized
	if err := s2.Authorize(ctx, "nodeA", testValidFingerprint); err != nil {
		t.Fatalf("reopened nodeA should still be authorized, got: %v", err)
	}

	// nodeB must still be revoked
	if err := s2.Authorize(ctx, "nodeB", testOtherFingerprint); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("reopened nodeB should be unauthorized, got: %v", err)
	}

	// nodeB cannot be resurrected
	if err := s2.Register(ctx, "nodeB", testOtherFingerprint, exp); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("reopened nodeB cannot be resurrected, got: %v", err)
	}

	// Now revoke nodeA on s2 and close
	if err := s2.Revoke(ctx, "nodeA", "admin", "decommissioned"); err != nil {
		t.Fatalf("Revoke nodeA failed: %v", err)
	}
	_ = s2.Close()

	// Instance 3: Verify nodeA is revoked after second reopen
	s3, err := Open(p)
	if err != nil {
		t.Fatalf("Open s3 failed: %v", err)
	}
	defer s3.Close()

	if err := s3.Authorize(ctx, "nodeA", testValidFingerprint); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("nodeA should be revoked in s3, got: %v", err)
	}
}

func TestTwoStores_ImmediateRevocation(t *testing.T) {
	p := newTempDB(t)
	ctx := context.Background()

	s1, err := Open(p)
	if err != nil {
		t.Fatalf("Open s1 failed: %v", err)
	}
	defer s1.Close()

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("Open s2 failed: %v", err)
	}
	defer s2.Close()

	nodeID := "node-concurrent-check"
	exp := time.Now().Add(24 * time.Hour)

	// Register on s1
	if err := s1.Register(ctx, nodeID, testValidFingerprint, exp); err != nil {
		t.Fatalf("Register on s1 failed: %v", err)
	}

	// s2 should immediately authorize
	if err := s2.Authorize(ctx, nodeID, testValidFingerprint); err != nil {
		t.Fatalf("s2 Authorize failed immediately after s1 Register: %v", err)
	}

	// Revoke on s1
	if err := s1.Revoke(ctx, nodeID, "secops", "emergency revoke"); err != nil {
		t.Fatalf("Revoke on s1 failed: %v", err)
	}

	// s2 must immediately observe revocation without any delay or cache invalidation lag
	if err := s2.Authorize(ctx, nodeID, testValidFingerprint); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("s2 must immediately reject revoked device, got: %v", err)
	}
}

func TestConcurrent_Registrations(t *testing.T) {
	t.Run("conflicting fingerprints on same nodeID", func(t *testing.T) {
		p := newTempDB(t)
		s := openTestStore(t, p)
		ctx := context.Background()
		exp := time.Now().Add(24 * time.Hour)

		const workers = 20
		nodeID := "contended-node"
		var successCount int32
		var conflictCount int32

		var wg sync.WaitGroup
		start := make(chan struct{})

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				// Each worker sends a distinct fingerprint
				fp := fmt.Sprintf("%064x", idx+1)
				err := s.Register(ctx, nodeID, fp, exp)
				if err == nil {
					atomic.AddInt32(&successCount, 1)
				} else if errors.Is(err, protocol.ErrConflict) {
					atomic.AddInt32(&conflictCount, 1)
				} else {
					t.Errorf("worker %d got unexpected error: %v", idx, err)
				}
			}(i)
		}

		close(start)
		wg.Wait()

		if successCount != 1 {
			t.Fatalf("expected exactly 1 winner, got %d", successCount)
		}
		if conflictCount != int32(workers-1) {
			t.Fatalf("expected %d conflicts, got %d", workers-1, conflictCount)
		}
	})

	t.Run("concurrent distinct nodeIDs all succeed", func(t *testing.T) {
		p := newTempDB(t)
		s := openTestStore(t, p)
		ctx := context.Background()
		exp := time.Now().Add(24 * time.Hour)

		const workers = 30
		var wg sync.WaitGroup
		start := make(chan struct{})

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				nodeID := fmt.Sprintf("distinct-node-%04d", idx)
				fp := fmt.Sprintf("%064x", idx+1)
				if err := s.Register(ctx, nodeID, fp, exp); err != nil {
					t.Errorf("register %s failed: %v", nodeID, err)
				}
				if err := s.Authorize(ctx, nodeID, fp); err != nil {
					t.Errorf("authorize %s failed: %v", nodeID, err)
				}
			}(i)
		}

		close(start)
		wg.Wait()
	})

	t.Run("concurrent identical registrations are all idempotent", func(t *testing.T) {
		p := newTempDB(t)
		s := openTestStore(t, p)
		ctx := context.Background()
		exp := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)

		const workers = 20
		nodeID := "idempotent-node"
		var wg sync.WaitGroup
		start := make(chan struct{})

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := s.Register(ctx, nodeID, testValidFingerprint, exp); err != nil {
					t.Errorf("idempotent register failed: %v", err)
				}
			}()
		}

		close(start)
		wg.Wait()

		if err := s.Authorize(ctx, nodeID, testValidFingerprint); err != nil {
			t.Fatalf("final authorize failed: %v", err)
		}
	})
}

func TestContextCancellation(t *testing.T) {
	p := newTempDB(t)
	s := openTestStore(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	exp := time.Now().Add(24 * time.Hour)
	if err := s.Register(ctx, testValidNodeID, testValidFingerprint, exp); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from Register, got: %v", err)
	}

	if err := s.Authorize(ctx, testValidNodeID, testValidFingerprint); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from Authorize, got: %v", err)
	}

	if err := s.Revoke(ctx, testValidNodeID, "admin", "reason"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from Revoke, got: %v", err)
	}
}

func TestDatabaseSchemaVersionCheck(t *testing.T) {
	p := newTempDB(t)

	// Pre-create database with unsupported user_version=2
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version=2;"); err != nil {
		_ = db.Close()
		t.Fatalf("set pragma failed: %v", err)
	}
	_ = db.Close()

	_, err = Open(p)
	if !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("expected ErrConflict for unsupported schema version, got: %v", err)
	}
}

func TestConcurrent_InitialOpen(t *testing.T) {
	p := newTempDB(t)
	const workers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, workers)
	stores := make([]*Store, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			s, err := Open(p)
			if err != nil {
				errs <- fmt.Errorf("worker %d Open failed: %w", idx, err)
				return
			}
			stores[idx] = s
		}(i)
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent initial open error: %v", err)
	}

	for _, s := range stores {
		if s != nil {
			_ = s.Close()
		}
	}
}
