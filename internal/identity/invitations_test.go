package identity

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func newTestStore(t *testing.T) *InvitationStore {
	t.Helper()
	store, err := OpenInvitationStore(filepath.Join(t.TempDir(), "invitations"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func TestInvitationIssueConsumeAndReplay(t *testing.T) {
	store := newTestStore(t)

	invitation, err := store.Issue(5*time.Minute, "the-fingerprint")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if invitation.Token == "" || invitation.ServerFingerprint != "the-fingerprint" {
		t.Fatalf("unexpected invitation: %+v", invitation)
	}
	if pending, err := store.Pending(); err != nil || pending != 1 {
		t.Fatalf("pending is %d (err %v), want 1", pending, err)
	}

	if err := store.Consume(invitation.Token); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if pending, err := store.Pending(); err != nil || pending != 0 {
		t.Fatalf("pending is %d (err %v) after use, want 0", pending, err)
	}
	if used, err := store.Used(); err != nil || used != 1 {
		t.Fatalf("used is %d (err %v), want 1", used, err)
	}

	// The whole point of a one-time token: a replay must not pair a second machine.
	if err := store.Consume(invitation.Token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("replayed token returned %v, want unauthorized", err)
	}
	// And an unknown token must be refused identically, without saying which case it is.
	if err := store.Consume("0123456789abcdef"); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("unknown token returned %v, want unauthorized", err)
	}
	if err := store.Consume(""); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("empty token returned %v, want unauthorized", err)
	}
}

func TestInvitationExpiry(t *testing.T) {
	store := newTestStore(t)

	// Mint with a clock an hour in the past, then check it with the real one.
	past := time.Now().Add(-time.Hour)
	store.now = func() time.Time { return past }
	invitation, err := store.Issue(time.Minute, "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	store.now = time.Now

	if err := store.Consume(invitation.Token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("expired token returned %v, want unauthorized", err)
	}
	if pending, err := store.Pending(); err != nil || pending != 0 {
		t.Fatalf("expired invitation still counts as pending: %d (err %v)", pending, err)
	}

	// A second mint prunes what the first left behind.
	if _, err := store.Issue(time.Minute, ""); err != nil {
		t.Fatalf("issue after expiry: %v", err)
	}
	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatalf("read store dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), pendingSuffix) && !isPendingFile(entry.Name()) {
			continue
		}
		if strings.Contains(entry.Name(), hashToken(invitation.Token)) {
			t.Fatal("the expired invitation was not pruned")
		}
	}
}

func TestInvitationTTLIsBounded(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Issue(MaxInvitationTTL+time.Second, ""); !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("an over-long ttl returned %v, want invalid", err)
	}
}

func TestInvitationSurvivesASeparateProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "invitations")

	minter, err := OpenInvitationStore(dir)
	if err != nil {
		t.Fatalf("open minter: %v", err)
	}
	invitation, err := minter.Issue(5*time.Minute, "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A different store object stands in for a gateway that was restarted, or
	// for the offline command that minted the token: neither shares memory.
	consumer, err := OpenInvitationStore(dir)
	if err != nil {
		t.Fatalf("open consumer: %v", err)
	}
	if err := consumer.Consume(invitation.Token); err != nil {
		t.Fatalf("consume from a second store: %v", err)
	}
}

func TestInvitationStoreWritesNoSecret(t *testing.T) {
	store := newTestStore(t)
	invitation, err := store.Issue(5*time.Minute, "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := store.Consume(invitation.Token); err != nil {
		t.Fatalf("consume: %v", err)
	}

	// Both records, pending and used, must stay hash-only and private: the data
	// directory is backed up and inspected by humans.
	scanned := 0
	err = filepath.WalkDir(store.Dir(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), invitation.Token) {
			t.Fatalf("%s contains the invitation token", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s has mode %o, want 600", path, perm)
		}
		scanned++
		return nil
	})
	if err != nil {
		t.Fatalf("walk store: %v", err)
	}
	if scanned == 0 {
		t.Fatal("no invitation files were checked")
	}
}

func TestCAAcceptsAnInvitationMintedBeforeItLoaded(t *testing.T) {
	dataDir := t.TempDir()

	first, err := LoadOrGenerateCA(dataDir)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	invitation, err := first.GenerateInvitation(5 * time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A gateway restart, or an offline 'mesh invite', mints for a data
	// directory both see: the token has to keep working.
	second, err := LoadOrGenerateCA(dataDir)
	if err != nil {
		t.Fatalf("reload CA: %v", err)
	}
	_, csrPEM, err := GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatalf("generate CSR: %v", err)
	}
	if _, _, err := second.SignNodeCSR(invitation.Token, csrPEM); err != nil {
		t.Fatalf("pairing with an invitation from the previous process: %v", err)
	}
}

func TestCAIssuesInvitationsWithoutThePrivateKey(t *testing.T) {
	dataDir := t.TempDir()
	ca, err := LoadOrGenerateCA(dataDir)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}

	// This is what 'mesh invite --data-dir' does for a gateway that is already
	// running: no CA key is loaded, and the running gateway accepts the token.
	invitation, err := IssueInvitation(dataDir, 5*time.Minute, nil)
	if err != nil {
		t.Fatalf("issue for a running gateway: %v", err)
	}
	if invitation.ServerFingerprint != ca.Fingerprint() {
		t.Fatalf("fingerprint is %q, want %q", invitation.ServerFingerprint, ca.Fingerprint())
	}

	_, csrPEM, err := GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatalf("generate CSR: %v", err)
	}
	if _, _, err := ca.SignNodeCSR(invitation.Token, csrPEM); err != nil {
		t.Fatalf("the running gateway refused an offline invitation: %v", err)
	}

	pending, err := PendingInvitations(dataDir)
	if err != nil || pending != 0 {
		t.Fatalf("pending is %d (err %v) after use, want 0", pending, err)
	}
}

func TestIssueInvitationNeedsAGatewayDirectory(t *testing.T) {
	// Minting into a directory that holds no CA would hand out a token no
	// gateway could ever accept, so it is refused instead.
	if _, err := IssueInvitation(t.TempDir(), time.Minute, nil); !errors.Is(err, protocol.ErrNotFound) {
		t.Fatalf("minting into an empty directory returned %v, want not found", err)
	}
}
