package policy

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// TestAuthenticateReturnsTheIssuedPrincipal: authentication reports the stored
// identity, scope included, and changes nothing.
func TestAuthenticateReturnsTheIssuedPrincipal(t *testing.T) {
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{"node-b", "node-a"}, testTime.Add(time.Hour))

	got := mustAuthenticate(t, st, token)
	if got.ID != p.ID || got.Role != p.Role || !reflect.DeepEqual(got.NodeIDs, p.NodeIDs) {
		t.Fatalf("Authenticate = %+v, want %+v", got, p)
	}
	// Authentication is a read: the scope is still there for the next caller.
	before := countPrincipals(t, st)
	if again := mustAuthenticate(t, st, token); again.ID != p.ID {
		t.Fatalf("second Authenticate = %+v, want %s", again, p.ID)
	}
	if after := countPrincipals(t, st); after != before {
		t.Fatalf("Authenticate changed the principal count from %d to %d", before, after)
	}
}

// TestAuthenticateRefusalsAreIndistinguishable is the no-oracle rule: unknown,
// expired and revoked credentials must produce the same sentinel *and* the same
// message, or an attacker holding a token could learn from the error whether it
// was ever issued.
func TestAuthenticateRefusalsAreIndistinguishable(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)

	// A revoked credential: issued, then withdrawn.
	revokedPrincipal, revokedToken := mustIssue(t, st, Viewer, []string{testNode}, testTime.Add(time.Hour))
	mustRevoke(t, st, revokedPrincipal.ID)

	// An expired one: issued and left alone until the clock moves past its
	// expiry. Moving the clock also carries the revoked credential past its
	// own expiry, so the two rows now differ in how they got here and must
	// still fail identically.
	_, expiredToken := mustIssue(t, st, Viewer, []string{testNode}, testTime.Add(time.Hour))
	freezeClock(st, testTime.Add(2*time.Hour))

	cases := map[string]string{
		"unknown token": "a-token-that-was-never-issued",
		"revoked token": revokedToken,
		"expired token": expiredToken,
	}
	var messages []string
	for name, token := range cases {
		p, err := st.Authenticate(ctx, token)
		if !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("%s = %v, want protocol.ErrUnauthorized", name, err)
		}
		if p.ID != "" || p.Role != "" || len(p.NodeIDs) != 0 {
			t.Fatalf("%s returned a principal %+v alongside an error", name, p)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("%s: the error reflects the token: %v", name, err)
		}
		messages = append(messages, err.Error())
	}
	for _, msg := range messages[1:] {
		if msg != messages[0] {
			t.Fatalf("refusals differ: %q and %q", messages[0], msg)
		}
	}
}

// TestAuthenticateRefusesMalformedTokens: a token that cannot be one of ours is
// refused the same way an unknown one is, without being hashed into a query.
func TestAuthenticateRefusesMalformedTokens(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	_, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))

	cases := map[string]string{
		"empty":            "",
		"blank":            "   ",
		"too long":         strings.Repeat("a", maxTokenBytes+1),
		"truncated":        token[:len(token)-1],
		"extended":         token + "a",
		"case flipped":     strings.ToUpper(token),
		"padded":           token + "=",
		"a prefix of it":   token[:8],
		"control char":     "abc\x00def",
		"not the same one": "not-a-token",
	}
	for name, candidate := range cases {
		if _, err := st.Authenticate(ctx, candidate); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Errorf("Authenticate(%s) = %v, want protocol.ErrUnauthorized", name, err)
		}
	}
	// The real token still works, so the refusals above were about the
	// candidates and not about a broken store.
	if got := mustAuthenticate(t, st, token); got.ID == "" {
		t.Fatal("the issued token stopped authenticating")
	}
}

// TestAuthenticateExpiryBoundary pins the edge: a credential is live until its
// expiry instant and not at it.
func TestAuthenticateExpiryBoundary(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	expiry := testTime.Add(time.Hour)
	_, token := mustIssue(t, st, Viewer, []string{testNode}, expiry)

	freezeClock(st, expiry.Add(-time.Nanosecond))
	if _, err := st.Authenticate(ctx, token); err != nil {
		t.Fatalf("Authenticate one nanosecond before expiry: %v", err)
	}
	freezeClock(st, expiry)
	if _, err := st.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Authenticate at the expiry instant = %v, want protocol.ErrUnauthorized", err)
	}
	freezeClock(st, expiry.Add(time.Nanosecond))
	if _, err := st.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Authenticate past expiry = %v, want protocol.ErrUnauthorized", err)
	}
}

// TestRevocationReachesAnotherHandleOpenOnTheSameFile is the two-instance
// requirement: a second Store sharing the database must see a revocation the
// moment it commits, because no handle caches a principal.
func TestRevocationReachesAnotherHandleOpenOnTheSameFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.db")
	first := openStoreAt(t, path)
	second := openStoreAt(t, path)
	freezeClock(first, testTime)
	freezeClock(second, testTime)

	p, token := mustIssue(t, first, Operator, []string{testNode}, testTime.Add(time.Hour))
	if got := mustAuthenticate(t, second, token); got.ID != p.ID {
		t.Fatalf("the second handle authenticated %s, want %s", got.ID, p.ID)
	}
	if err := Authorize(mustAuthenticate(t, second, token), actionTaskSubmit, testNode); err != nil {
		t.Fatalf("the operator should be allowed to submit: %v", err)
	}

	mustRevoke(t, first, p.ID)

	// The revocation is visible to the other handle without it reopening
	// anything, and the principal it can no longer authenticate is gone with
	// its authority.
	if _, err := second.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("the second handle still accepts a revoked token: %v", err)
	}
	// And the handle that issued the revocation agrees with itself.
	if _, err := first.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("the revoking handle still accepts the revoked token: %v", err)
	}
}

// TestAuthenticateSurvivesReopen: a credential outlives the process that issued
// it, which is the whole point of storing it.
func TestAuthenticateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	st := openStoreAt(t, path)
	freezeClock(st, testTime)
	p, token := mustIssue(t, st, Operator, []string{"node-b", "node-a"}, testTime.Add(time.Hour))
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openStoreAt(t, path)
	freezeClock(reopened, testTime)
	got := mustAuthenticate(t, reopened, token)
	if got.ID != p.ID || !reflect.DeepEqual(got.NodeIDs, []string{"node-a", "node-b"}) {
		t.Fatalf("after reopen = %+v, want %+v", got, p)
	}
	if err := Authorize(got, actionTaskCancel, "node-a"); err != nil {
		t.Fatalf("the reopened principal lost its scope: %v", err)
	}
}

// TestAuthenticateIsScopedToItsOwnDatabase: two stores with different files
// share nothing, so a token from one is simply unknown to the other.
func TestAuthenticateIsScopedToItsOwnDatabase(t *testing.T) {
	ctx := context.Background()
	one := newFrozenStore(t)
	other := newFrozenStore(t)
	_, token := mustIssue(t, one, Operator, []string{testNode}, testTime.Add(time.Hour))

	if _, err := other.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("a token from another database = %v, want protocol.ErrUnauthorized", err)
	}
}

// TestAuthenticateAfterCloseFails: a closed store reports a storage failure,
// which is not the same thing as a rejected credential — the caller must be
// able to tell a broken gateway from a bad token.
func TestAuthenticateAfterCloseFails(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	_, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_, err := st.Authenticate(ctx, token)
	if err == nil {
		t.Fatal("Authenticate after Close succeeded")
	}
	if errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("a storage failure is reported as a rejected credential: %v", err)
	}
}
