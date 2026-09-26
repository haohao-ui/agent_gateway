package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// TestTokenIsNeverStoredInPlaintext scans every byte SQLite wrote for the
// token. Storing a hash in the principals table is not enough on its own: the
// token must not appear in a scope row, the WAL, or a leftover page either.
func TestTokenIsNeverStoredInPlaintext(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.db")
	st := openStoreAt(t, path)
	freezeClock(st, testTime)

	operator, operatorToken := mustIssue(t, st, Operator, []string{"node-b", "node-a"}, testTime.Add(time.Hour))
	_, revokedToken := mustIssue(t, st, Viewer, []string{testNode}, testTime.Add(time.Hour))
	_, adminToken := mustIssue(t, st, Admin, nil, testTime.Add(time.Hour))
	// Revoke one, so the revocation path is written to the WAL too.
	mustRevoke(t, st, operator.ID)
	if _, err := st.Authenticate(ctx, operatorToken); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("the revoked token was accepted: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tokens := map[string]string{
		"operator": operatorToken,
		"revoked":  revokedToken,
		"admin":    adminToken,
	}
	files := readDatabaseFiles(t, path)
	t.Logf("scanning %d database files for %d tokens", len(files), len(tokens))
	for name, data := range files {
		for label, token := range tokens {
			if bytes.Contains(data, []byte(token)) {
				t.Fatalf("%s contains the %s token in plaintext", name, label)
			}
		}
	}

	// Positive controls, so a scan that read the wrong files cannot pass. The
	// token's hash must be present — that is what the database is supposed to
	// hold — and so must the principal id, which proves these are the files
	// this store wrote.
	hash := []byte(tokenHash(operatorToken))
	id := []byte(operator.ID)
	found := false
	for name, data := range files {
		if !bytes.Contains(data, id) {
			continue
		}
		found = true
		if !bytes.Contains(data, hash) {
			t.Errorf("%s holds the principal id but not the token hash", name)
		}
	}
	if !found {
		t.Fatal("no database file contains the principal id, so the scan proves nothing")
	}
}

// TestTokenIsAbsentFromPrincipalAndErrors checks the other half of the
// contract: the secret is returned once and never appears in a principal, in
// JSON, or in the text of an error.
func TestTokenIsAbsentFromPrincipalAndErrors(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))
	authenticated := mustAuthenticate(t, st, token)

	encoded, err := json.Marshal(authenticated)
	if err != nil {
		t.Fatalf("marshal the principal: %v", err)
	}
	if bytes.Contains(encoded, []byte(token)) {
		t.Fatal("the principal JSON contains the token")
	}
	if bytes.Contains(encoded, []byte(tokenHash(token))) {
		t.Fatal("the principal JSON contains the token hash")
	}

	// Every failure a caller can provoke must be free of the token, whether it
	// is the real one or a wrong one the caller supplied.
	wrong := "wrong-" + token
	_, authErr := st.Authenticate(ctx, wrong)
	revokeErr := st.Revoke(ctx, wrong)
	authorizeErrs := []error{
		Authorize(Principal{ID: p.ID, Role: p.Role}, actionTaskSubmit, testNode),
		Authorize(Principal{ID: p.ID, Role: p.Role, NodeIDs: p.NodeIDs}, actionTaskSubmit, otherNode),
		Authorize(Principal{ID: p.ID, Role: p.Role, NodeIDs: p.NodeIDs}, "task.delete", testNode),
	}
	for _, err := range append(authorizeErrs, authErr, revokeErr) {
		if err == nil {
			t.Fatal("expected a failure to inspect")
		}
		for _, secret := range []string{token, tokenHash(token), wrong} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the error leaks %q: %v", secret, err)
			}
		}
		if errors.Is(err, protocol.ErrUnauthorized) && strings.Contains(err.Error(), p.ID) {
			t.Fatalf("an authentication refusal names the principal: %v", err)
		}
	}
}

// TestTheStoredHashIsNotTheToken is the one-line version of the scan above,
// stated as a property: what the database holds for a credential is its
// SHA-256, and the token itself is not derivable from it.
func TestTheStoredHashIsNotTheToken(t *testing.T) {
	st := newFrozenStore(t)
	_, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))
	hash := tokenHash(token)
	if hash == token {
		t.Fatal("the stored value is the token itself")
	}
	if len(hash) != 64 {
		t.Fatalf("stored hash is %d characters, want 64 lowercase hex", len(hash))
	}
	if strings.ToLower(hash) != hash {
		t.Fatalf("stored hash %q is not lowercase hex", hash)
	}
	if n := storedHashCount(t, st, hash); n != 1 {
		t.Fatalf("rows for the token hash = %d, want 1", n)
	}
	// A token that differs by one character is a different credential, which
	// is what makes the hash a lookup key rather than a fuzzy match.
	other := token[:len(token)-1] + "x"
	if other != token && storedHashCount(t, st, tokenHash(other)) != 0 {
		t.Fatal("a token that differs by one character collided with the real one")
	}
}
