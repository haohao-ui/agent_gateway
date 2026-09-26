package policy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// TestConcurrentIssuesProduceDistinctPrincipals runs the write path from many
// goroutines at once. Every issue must succeed, and no two may share an
// identifier or a token — a collision would mean two credentials for one
// identity, or one identity that revoking would take away from two callers.
func TestConcurrentIssuesProduceDistinctPrincipals(t *testing.T) {
	st := newFrozenStore(t)
	const writers = 32

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ids      = make(map[string]bool, writers)
		tokens   = make(map[string]bool, writers)
		failures = make(chan error, writers)
	)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			role := Operator
			scope := []string{nodeName(i)}
			p, token, err := st.Issue(context.Background(), role, scope, testTime.Add(time.Hour))
			if err != nil {
				failures <- fmt.Errorf("Issue %d: %w", i, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ids[p.ID] {
				failures <- fmt.Errorf("duplicate principal id %s", p.ID)
			}
			if tokens[token] {
				failures <- fmt.Errorf("duplicate token for %s", p.ID)
			}
			ids[p.ID] = true
			tokens[token] = true
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if len(ids) != writers {
		t.Fatalf("%d distinct principals, want %d", len(ids), writers)
	}
	if n := countPrincipals(t, st); n != writers {
		t.Fatalf("stored principals = %d, want %d", n, writers)
	}
	// Every one of them authenticates, in a second pass over the same store.
	mu.Lock()
	defer mu.Unlock()
	for token := range tokens {
		if _, err := st.Authenticate(context.Background(), token); err != nil {
			t.Errorf("a concurrently issued token does not authenticate: %v", err)
		}
	}
}

// TestAuthenticationDuringRevocation hammers the read path while a revocation
// commits. Readers may see the credential live or gone, but nothing else: a
// transient storage error under concurrency would mean a valid operator gets
// locked out by someone else's revocation. Once Revoke has returned, the
// credential must be refused by every subsequent call.
func TestAuthenticationDuringRevocation(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))

	const readers = 16
	var (
		wg        sync.WaitGroup
		successes int64
		failures  = make(chan error, readers)
		stop      = make(chan struct{})
		refusals  int64
	)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := st.Authenticate(ctx, token)
				switch {
				case err == nil:
					if got.ID != p.ID {
						failures <- fmt.Errorf("authenticated as %s, want %s", got.ID, p.ID)
						return
					}
					atomic.AddInt64(&successes, 1)
				case errors.Is(err, protocol.ErrUnauthorized):
					atomic.AddInt64(&refusals, 1)
				default:
					failures <- fmt.Errorf("unexpected authentication error: %w", err)
					return
				}
			}
		}()
	}

	// Wait for the readers to be genuinely running before revoking, and for
	// one of them to notice the revocation afterwards. Both directions are
	// what make this test evidence rather than a race that happens to pass.
	waitUntil(t, "a reader to authenticate before the revocation", func() bool {
		return atomic.LoadInt64(&successes) > 0
	})
	mustRevoke(t, st, p.ID)
	waitUntil(t, "a reader to observe the revocation", func() bool {
		return atomic.LoadInt64(&refusals) > 0
	})
	close(stop)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	for i := 0; i < readers; i++ {
		if _, err := st.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("authenticate after the revocation returned: %v", err)
		}
	}
}

// TestConcurrentRevocationsOfOnePrincipal: the second revoke finds the row
// already revoked, so it must succeed without rewriting the first timestamp.
func TestConcurrentRevocationsOfOnePrincipal(t *testing.T) {
	st := newFrozenStore(t)
	p, _ := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))

	const revokers = 16
	var (
		wg       sync.WaitGroup
		failures = make(chan error, revokers)
	)
	for i := 0; i < revokers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Revoke(context.Background(), p.ID); err != nil {
				failures <- fmt.Errorf("Revoke: %w", err)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	revoked := storedRevokedAt(t, st, p.ID)
	if !revoked.Valid {
		t.Fatal("concurrent revocations recorded nothing")
	}
	if revoked.String != formatTime(testTime) {
		t.Fatalf("revoked_at = %q, want the frozen clock's %q", revoked.String, formatTime(testTime))
	}
}

// TestConcurrentIssueAndRevokeOfDistinctPrincipals mixes both writes: every
// principal must end up revoked, and no issue may fail because another
// goroutine was holding the write lock.
func TestConcurrentIssueAndRevokeOfDistinctPrincipals(t *testing.T) {
	st := newFrozenStore(t)
	const workers = 16

	var (
		wg       sync.WaitGroup
		failures = make(chan error, workers)
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, _, err := st.Issue(context.Background(), Viewer, []string{nodeName(i)}, testTime.Add(time.Hour))
			if err != nil {
				failures <- fmt.Errorf("Issue %d: %w", i, err)
				return
			}
			if err := st.Revoke(context.Background(), p.ID); err != nil {
				failures <- fmt.Errorf("Revoke %s: %w", p.ID, err)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if n := countPrincipals(t, st); n != workers {
		t.Fatalf("stored principals = %d, want %d", n, workers)
	}
	var live int
	ctx := context.Background()
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM principals WHERE revoked_at IS NULL`).Scan(&live); err != nil {
		t.Fatalf("count live principals: %v", err)
	}
	if live != 0 {
		t.Fatalf("%d principals are still live, want every one revoked", live)
	}
}

// TestConcurrentAuthenticationAcrossHandles: two handles on one file, readers
// on one and a revocation on the other. This is the closest thing to the
// two-process case that a single test binary can run.
func TestConcurrentAuthenticationAcrossHandles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.db")
	writer := openStoreAt(t, path)
	reader := openStoreAt(t, path)
	freezeClock(writer, testTime)
	freezeClock(reader, testTime)

	p, token := mustIssue(t, writer, Operator, []string{testNode}, testTime.Add(time.Hour))

	var (
		wg        sync.WaitGroup
		successes int64
		failures  = make(chan error, 8)
		stop      = make(chan struct{})
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := reader.Authenticate(ctx, token); err != nil {
					if errors.Is(err, protocol.ErrUnauthorized) {
						continue
					}
					failures <- fmt.Errorf("reader: %w", err)
					return
				}
				atomic.AddInt64(&successes, 1)
			}
		}()
	}
	waitUntil(t, "a reader to see the credential live", func() bool {
		return atomic.LoadInt64(&successes) > 0
	})
	mustRevoke(t, writer, p.ID)
	close(stop)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if _, err := reader.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("the reader handle accepts a token revoked by the other handle: %v", err)
	}
}
