package policy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// TestRevokeUnknownPrincipal: a principal that was never issued cannot be
// revoked, and unlike an authentication failure there is nothing to hide here.
func TestRevokeUnknownPrincipal(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	if err := st.Revoke(ctx, "prn_00000000000000000000000000000000"); !errors.Is(err, protocol.ErrNotFound) {
		t.Fatalf("Revoke of an unknown principal = %v, want protocol.ErrNotFound", err)
	}
	// A revoked principal stays revocable-looking but is still found: the row
	// is what makes the second call idempotent rather than a not-found.
	p, _ := mustIssue(t, st, Viewer, []string{testNode}, testTime.Add(time.Hour))
	mustRevoke(t, st, p.ID)
	if err := st.Revoke(ctx, p.ID); err != nil {
		t.Fatalf("second Revoke(%s): %v", p.ID, err)
	}
}

// TestRevokeValidatesTheID: the identifier is checked before any statement runs.
func TestRevokeValidatesTheID(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	cases := map[string]string{
		"empty":         "",
		"too long":      strings.Repeat("p", maxIDBytes+1),
		"control char":  "prn_\x001",
		"not UTF-8":     string([]byte{0xff, 0xfe}),
		"a tab inside":  "prn_\t1",
		"a null inside": "prn_\x00",
	}
	for name, id := range cases {
		if err := st.Revoke(ctx, id); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Revoke(%s) = %v, want protocol.ErrInvalid", name, err)
		}
	}
	// An id at the limit is accepted, even though no such principal exists.
	atLimit := "prn_" + strings.Repeat("a", maxIDBytes-len("prn_"))
	if err := st.Revoke(ctx, atLimit); !errors.Is(err, protocol.ErrNotFound) {
		t.Fatalf("Revoke at the id limit = %v, want protocol.ErrNotFound", err)
	}
}

// TestRevokeIsIdempotentAndKeepsTheFirstTimestamp: revoking twice succeeds, and
// the second call does not rewrite when the revocation happened — the timestamp
// is evidence, not a counter.
func TestRevokeIsIdempotentAndKeepsTheFirstTimestamp(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))

	if err := st.Revoke(ctx, p.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	first := storedRevokedAt(t, st, p.ID)
	if !first.Valid {
		t.Fatal("Revoke did not record a revoked_at")
	}

	// Move the clock, then revoke again: the recorded instant must not move.
	freezeClock(st, testTime.Add(time.Hour))
	if err := st.Revoke(ctx, p.ID); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	second := storedRevokedAt(t, st, p.ID)
	if second.String != first.String {
		t.Fatalf("the second revocation rewrote the timestamp: %q then %q", first.String, second.String)
	}
	if _, err := st.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("the revoked token was accepted: %v", err)
	}
}

// TestRevokeKeepsTheRow: the principal is withdrawn, not deleted, so "was this
// credential issued?" stays answerable.
func TestRevokeKeepsTheRow(t *testing.T) {
	st := newFrozenStore(t)
	p, _ := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))
	mustRevoke(t, st, p.ID)
	if n := countPrincipals(t, st); n != 1 {
		t.Fatalf("principals after revocation = %d, want the row kept", n)
	}
	if n := countScopes(t, st, p.ID); n != 1 {
		t.Fatalf("scope rows after revocation = %d, want 1: the scope is not the credential", n)
	}
}

// TestRevokeDoesNotTouchOtherPrincipals: revocation is per credential.
func TestRevokeDoesNotTouchOtherPrincipals(t *testing.T) {
	st := newFrozenStore(t)
	expiry := testTime.Add(time.Hour)
	one, oneToken := mustIssue(t, st, Operator, []string{testNode}, expiry)
	two, twoToken := mustIssue(t, st, Operator, []string{testNode}, expiry)
	admin, adminToken := mustIssue(t, st, Admin, nil, expiry)

	mustRevoke(t, st, two.ID)

	if mustAuthenticate(t, st, oneToken).ID != one.ID {
		t.Fatal("revoking one principal affected another")
	}
	if mustAuthenticate(t, st, adminToken).ID != admin.ID {
		t.Fatal("revoking an operator affected the admin")
	}
	if _, err := st.Authenticate(context.Background(), twoToken); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("the revoked token = %v, want protocol.ErrUnauthorized", err)
	}
}

// TestRevokeAcceptsAnExpiredPrincipal: an expired credential is still a row, and
// withdrawing it is meaningful — it is what stops a client from retrying a
// token whose expiry was the only thing that had ended it.
func TestRevokeAcceptsAnExpiredPrincipal(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Minute))
	freezeClock(st, testTime.Add(time.Hour))
	if _, err := st.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("the expired token = %v, want protocol.ErrUnauthorized", err)
	}
	if err := st.Revoke(ctx, p.ID); err != nil {
		t.Fatalf("Revoke of an expired principal: %v", err)
	}
	if revoked := storedRevokedAt(t, st, p.ID); !revoked.Valid {
		t.Fatal("Revoke of an expired principal recorded nothing")
	}
}

// TestRevocationSurvivesReopen: a revocation is durable, so restarting the
// gateway cannot resurrect a credential.
func TestRevocationSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	st := openStoreAt(t, path)
	freezeClock(st, testTime)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))
	mustRevoke(t, st, p.ID)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openStoreAt(t, path)
	freezeClock(reopened, testTime)
	if _, err := reopened.Authenticate(context.Background(), token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("after reopen the revoked token = %v, want protocol.ErrUnauthorized", err)
	}
}

// TestRevokeHonoursTheContext: a caller that gives up must not leave a
// revocation half-applied.
func TestRevokeHonoursTheContext(t *testing.T) {
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := st.Revoke(cancelled, p.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Revoke with a cancelled context = %v, want context.Canceled", err)
	}
	if revoked := storedRevokedAt(t, st, p.ID); revoked.Valid {
		t.Fatal("a cancelled Revoke still wrote a revocation")
	}
	if _, err := st.Authenticate(context.Background(), token); err != nil {
		t.Fatalf("the token should still be live: %v", err)
	}

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if err := st.Revoke(expired, p.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Revoke with an expired deadline = %v, want context.DeadlineExceeded", err)
	}
	if revoked := storedRevokedAt(t, st, p.ID); revoked.Valid {
		t.Fatal("a Revoke with an expired deadline still wrote a revocation")
	}
}

// TestRevokeAfterCloseFails: a closed store cannot pretend a revocation
// happened.
func TestRevokeAfterCloseFails(t *testing.T) {
	st := newFrozenStore(t)
	p, _ := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Revoke(context.Background(), p.ID); err == nil {
		t.Fatal("Revoke after Close succeeded")
	}
}
