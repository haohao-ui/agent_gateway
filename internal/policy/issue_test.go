package policy

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// TestIssueReturnsAGeneratedPrincipalAndToken checks the shape of what Issue
// hands back: a server-generated ID, the role and canonical scope, and a token
// that is 256 bits of randomness.
func TestIssueReturnsAGeneratedPrincipalAndToken(t *testing.T) {
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))

	if !strings.HasPrefix(p.ID, principalIDPrefix) || len(p.ID) != len(principalIDPrefix)+2*idRandomBytes {
		t.Fatalf("principal id = %q, want %q plus %d hex characters", p.ID, principalIDPrefix, 2*idRandomBytes)
	}
	if p.Role != Operator {
		t.Fatalf("role = %q, want %q", p.Role, Operator)
	}
	if !reflect.DeepEqual(p.NodeIDs, []string{testNode}) {
		t.Fatalf("scope = %v, want [%s]", p.NodeIDs, testNode)
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if len(raw) != tokenRandomBytes {
		t.Fatalf("token carries %d bytes of randomness, want %d", len(raw), tokenRandomBytes)
	}
	if strings.ContainsAny(token, "+/=") {
		t.Fatalf("token is not URL-safe: %q", token)
	}
	if n := storedHashCount(t, st, tokenHash(token)); n != 1 {
		t.Fatalf("stored rows for the token hash = %d, want 1", n)
	}
}

// TestIssueGivesEveryPrincipalItsOwnIdentity: two issues of the same shape must
// still be two identities with two credentials, or revoking one would revoke
// both.
func TestIssueGivesEveryPrincipalItsOwnIdentity(t *testing.T) {
	st := newFrozenStore(t)
	expiry := testTime.Add(time.Hour)
	first, firstToken := mustIssue(t, st, Viewer, []string{testNode}, expiry)
	second, secondToken := mustIssue(t, st, Viewer, []string{testNode}, expiry)

	if first.ID == second.ID {
		t.Fatalf("two issues produced the same principal id %q", first.ID)
	}
	if firstToken == secondToken {
		t.Fatal("two issues produced the same token")
	}
	if got := mustAuthenticate(t, st, firstToken); got.ID != first.ID {
		t.Fatalf("first token authenticated as %s, want %s", got.ID, first.ID)
	}
	if got := mustAuthenticate(t, st, secondToken); got.ID != second.ID {
		t.Fatalf("second token authenticated as %s, want %s", got.ID, second.ID)
	}
	// Revoking one must leave the other alone.
	mustRevoke(t, st, first.ID)
	if _, err := st.Authenticate(context.Background(), firstToken); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("revoked token = %v, want protocol.ErrUnauthorized", err)
	}
	if got := mustAuthenticate(t, st, secondToken); got.ID != second.ID {
		t.Fatalf("revoking one principal affected another: %s", got.ID)
	}
}

// TestIssueCanonicalisesTheScope: the stored scope is deduplicated and sorted,
// so two issues that name the same nodes in a different order and with repeats
// produce principals that compare equal.
func TestIssueCanonicalisesTheScope(t *testing.T) {
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{"node-c", "node-a", "node-b", "node-a"}, testTime.Add(time.Hour))
	want := []string{"node-a", "node-b", "node-c"}
	if !reflect.DeepEqual(p.NodeIDs, want) {
		t.Fatalf("scope = %v, want %v", p.NodeIDs, want)
	}
	if n := countScopes(t, st, p.ID); n != len(want) {
		t.Fatalf("stored scope rows = %d, want %d", n, len(want))
	}
	// What was stored is what Authenticate returns.
	got := mustAuthenticate(t, st, token)
	if !reflect.DeepEqual(got.NodeIDs, want) {
		t.Fatalf("authenticated scope = %v, want %v", got.NodeIDs, want)
	}
}

// TestIssueAdminHasNoScopeRows: an admin is global, and "global" is represented
// by the absence of scope rows, not by a wildcard row.
func TestIssueAdminHasNoScopeRows(t *testing.T) {
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Admin, nil, testTime.Add(time.Hour))
	if len(p.NodeIDs) != 0 {
		t.Fatalf("admin scope = %v, want empty", p.NodeIDs)
	}
	if n := countScopes(t, st, p.ID); n != 0 {
		t.Fatalf("admin has %d scope rows, want 0", n)
	}
	got := mustAuthenticate(t, st, token)
	if got.Role != Admin || len(got.NodeIDs) != 0 {
		t.Fatalf("authenticated admin = %+v, want an empty scope", got)
	}
	// An empty slice and a nil slice are the same scope; only the length is
	// contractual.
	if err := Authorize(got, actionDeviceRevoke, ""); err != nil {
		t.Fatalf("admin may revoke devices: %v", err)
	}
}

// TestIssueValidatesTheScope walks the scope rules: a role that needs a scope
// must have one, an admin must not, and every node id must be a bounded,
// printable, wildcard-free string.
func TestIssueValidatesTheScope(t *testing.T) {
	long := strings.Repeat("a", maxNodeIDBytes+1)
	cases := map[string]struct {
		role    Role
		nodeIDs []string
	}{
		"admin with a scope":             {role: Admin, nodeIDs: []string{testNode}},
		"admin with an empty entry":      {role: Admin, nodeIDs: []string{""}},
		"operator without a scope":       {role: Operator, nodeIDs: nil},
		"operator with an empty list":    {role: Operator, nodeIDs: []string{}},
		"viewer without a scope":         {role: Viewer, nodeIDs: nil},
		"empty node id":                  {role: Operator, nodeIDs: []string{"", testNode}},
		"node id too long":               {role: Operator, nodeIDs: []string{testNode, long}},
		"node id with a control char":    {role: Operator, nodeIDs: []string{"node\x001"}},
		"node id with a newline":         {role: Operator, nodeIDs: []string{"node\n1"}},
		"node id with a delete char":     {role: Operator, nodeIDs: []string{"node\x7f1"}},
		"node id that is not UTF-8":      {role: Operator, nodeIDs: []string{string([]byte{0xff, 0xfe})}},
		"node id with a star":            {role: Operator, nodeIDs: []string{"*"}},
		"node id with a star suffix":     {role: Operator, nodeIDs: []string{"node-*"}},
		"node id with a question":        {role: Operator, nodeIDs: []string{"node-?"}},
		"node id with a question inside": {role: Operator, nodeIDs: []string{"node-1?2"}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st := newFrozenStore(t)
			_, _, err := st.Issue(context.Background(), tc.role, tc.nodeIDs, testTime.Add(time.Hour))
			if !errors.Is(err, protocol.ErrInvalid) {
				t.Fatalf("Issue(%s, %v) = %v, want protocol.ErrInvalid", tc.role, tc.nodeIDs, err)
			}
			// A refused issue must leave nothing behind: no principal row and
			// no scope rows.
			if n := countPrincipals(t, st); n != 0 {
				t.Fatalf("a refused Issue stored %d principals", n)
			}
		})
	}
}

// TestIssueRejectsUnknownRoles: an unrecognised role is not a default, it is an
// error.
func TestIssueRejectsUnknownRoles(t *testing.T) {
	for _, role := range []Role{"", "superuser", "ADMIN", "Admin", "owner"} {
		st := newFrozenStore(t)
		_, _, err := st.Issue(context.Background(), role, []string{testNode}, testTime.Add(time.Hour))
		if !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Issue(%q) = %v, want protocol.ErrInvalid", role, err)
		}
		if strings.Contains(err.Error(), string(role)) && role != "" {
			t.Errorf("the error reflects the role value: %v", err)
		}
	}
}

// TestIssueBoundsTheScopeSize: at most maxScopeNodes nodes, counted before
// deduplication, so repeats cannot smuggle a longer list past the limit.
func TestIssueBoundsTheScopeSize(t *testing.T) {
	st := newFrozenStore(t)
	expiry := testTime.Add(time.Hour)

	atLimit := make([]string, 0, maxScopeNodes)
	for i := 0; i < maxScopeNodes; i++ {
		atLimit = append(atLimit, nodeName(i))
	}
	if _, _, err := st.Issue(context.Background(), Operator, atLimit, expiry); err != nil {
		t.Fatalf("Issue with %d nodes: %v", maxScopeNodes, err)
	}

	overLimit := append(atLimit, nodeName(maxScopeNodes))
	if _, _, err := st.Issue(context.Background(), Operator, overLimit, expiry); !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("Issue with %d nodes = %v, want protocol.ErrInvalid", len(overLimit), err)
	}

	repeated := make([]string, maxScopeNodes*2)
	for i := range repeated {
		repeated[i] = testNode
	}
	if _, _, err := st.Issue(context.Background(), Operator, repeated, expiry); !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("Issue with %d repeated nodes = %v, want protocol.ErrInvalid", len(repeated), err)
	}
}

// TestIssueBoundsTheValidityWindow checks the expiry rule on both edges: it
// must be in the future, and at most 365 days out.
func TestIssueBoundsTheValidityWindow(t *testing.T) {
	cases := map[string]struct {
		expiresAt time.Time
		wantErr   bool
	}{
		"zero":                 {expiresAt: time.Time{}, wantErr: true},
		"one nanosecond ago":   {expiresAt: testTime.Add(-time.Nanosecond), wantErr: true},
		"an hour ago":          {expiresAt: testTime.Add(-time.Hour), wantErr: true},
		"exactly now":          {expiresAt: testTime, wantErr: true},
		"one nanosecond ahead": {expiresAt: testTime.Add(time.Nanosecond)},
		"one hour ahead":       {expiresAt: testTime.Add(time.Hour)},
		"365 days ahead":       {expiresAt: testTime.Add(maxValidity)},
		"365 days plus a bit":  {expiresAt: testTime.Add(maxValidity + time.Nanosecond), wantErr: true},
		"366 days ahead":       {expiresAt: testTime.Add(366 * 24 * time.Hour), wantErr: true},
		"416 days ahead":       {expiresAt: testTime.Add(10000 * time.Hour), wantErr: true},
		"a century ahead":      {expiresAt: testTime.AddDate(100, 0, 0), wantErr: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st := newFrozenStore(t)
			_, _, err := st.Issue(context.Background(), Operator, []string{testNode}, tc.expiresAt)
			if tc.wantErr {
				if !errors.Is(err, protocol.ErrInvalid) {
					t.Fatalf("Issue(expires %s) = %v, want protocol.ErrInvalid", tc.expiresAt, err)
				}
				if n := countPrincipals(t, st); n != 0 {
					t.Fatalf("a refused Issue stored %d principals", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("Issue(expires %s): %v", tc.expiresAt, err)
			}
			if n := countPrincipals(t, st); n != 1 {
				t.Fatalf("stored principals = %d, want 1", n)
			}
		})
	}
}

// TestIssueMeasuresTheValidityWindowAfterTakingTheLock pins the convention that
// the clock is read under the write lock. The credential asked for here is
// valid when the call is made and expired by the time the lock is released, so
// an implementation that read the clock before waiting would accept it — and
// would issue a credential whose whole validity had already been spent waiting.
func TestIssueMeasuresTheValidityWindowAfterTakingTheLock(t *testing.T) {
	st := newStore(t)
	before := testTime
	after := testTime.Add(2 * time.Minute)

	var issueErr error
	runWhileLockHeld(t, st, before, after, func() {
		_, _, issueErr = st.Issue(context.Background(), Operator, []string{testNode}, before.Add(time.Minute))
	})

	if !errors.Is(issueErr, protocol.ErrInvalid) {
		t.Fatalf("Issue with an expiry that lapsed while waiting for the lock = %v, want protocol.ErrInvalid", issueErr)
	}
	if n := countPrincipals(t, st); n != 0 {
		t.Fatalf("the refused Issue stored %d principals", n)
	}
}

// TestIssueHonoursTheContext: a cancelled or expired context must stop the
// mutation rather than leave it to finish behind the caller's back.
func TestIssueHonoursTheContext(t *testing.T) {
	st := newFrozenStore(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := st.Issue(cancelled, Operator, []string{testNode}, testTime.Add(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Issue with a cancelled context = %v, want context.Canceled", err)
	}

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, _, err := st.Issue(expired, Operator, []string{testNode}, testTime.Add(time.Hour)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Issue with an expired deadline = %v, want context.DeadlineExceeded", err)
	}

	if n := countPrincipals(t, st); n != 0 {
		t.Fatalf("a refused Issue stored %d principals", n)
	}
}

// TestIssueErrorMessagesAreStable checks the error shape a caller branches on:
// validation failures are protocol.ErrInvalid, and the message names the field
// without echoing an unbounded value.
func TestIssueErrorMessagesAreStable(t *testing.T) {
	st := newFrozenStore(t)
	_, _, err := st.Issue(context.Background(), Operator, nil, testTime.Add(time.Hour))
	if err == nil {
		t.Fatal("Issue without a scope succeeded")
	}
	if !strings.HasPrefix(err.Error(), "policy: ") {
		t.Fatalf("error %q does not name the package", err)
	}
	if !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("error %v does not wrap protocol.ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Fatalf("error %q does not name the field that failed", err)
	}
}
