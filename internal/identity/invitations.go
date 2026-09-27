package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-gateway/internal/protocol"
)

const (
	invitationDirName = "invitations"
	pendingSuffix     = ".json"
	usedSuffix        = ".used.json"
	invitationBytes   = 24

	// MaxInvitationTTL bounds how long a pairing secret can stay usable. An
	// invitation is a bearer secret that anyone holding it can turn into a
	// device certificate, so it is meant to be short lived.
	MaxInvitationTTL = 24 * time.Hour
)

// invitationRecord is what a stored invitation contains. The token itself is
// never written: only its hash, so a stolen data directory cannot be turned
// into a pairing attempt.
type invitationRecord struct {
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedAt    time.Time `json:"used_at,omitzero"`
	MaxUses   int       `json:"max_uses,omitempty"`
	UseCount  int       `json:"use_count,omitempty"`

	// pending says which file the record was read from, so the summaries can
	// tell a spendable record from the record of a spent one. It is not stored.
	pending bool
}

// InvitationStore keeps pairing invitations in one directory.
//
// Each invitation is its own file, which is what makes the two operations that
// happen concurrently — minting and consuming — safe without a lock: minting
// only ever creates a new file, and consuming only ever touches the one file
// whose name is the presented token's hash. Consumption is a rename, so two
// gateways sharing a directory cannot both accept the same invitation.
type InvitationStore struct {
	dir string
	now func() time.Time
}

// OpenInvitationStore prepares the invitation directory.
func OpenInvitationStore(dir string) (*InvitationStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create invitation directory: %w", err)
	}
	return &InvitationStore{dir: dir, now: time.Now}, nil
}

// Dir returns the directory the store reads and writes.
func (s *InvitationStore) Dir() string { return s.dir }

// Issue mints one invitation valid for ttl and returns the secret. The secret
// is returned exactly once, here: only its hash reaches the disk.
//
// A non-positive ttl produces an already expired invitation, which is how a
// caller creates a record that must not be accepted.
func (s *InvitationStore) Issue(ttl time.Duration, serverFingerprint string) (protocol.PairInvitation, error) {
	return s.IssueWithUses(ttl, 1, serverFingerprint)
}

// IssueWithUses mints an invitation valid for ttl and a specified number of uses.
// maxUses: 1 means single-use (one-machine limit), >1 means multi-use up to maxUses,
// -1 means unlimited uses within ttl.
func (s *InvitationStore) IssueWithUses(ttl time.Duration, maxUses int, serverFingerprint string) (protocol.PairInvitation, error) {
	if ttl > MaxInvitationTTL {
		return protocol.PairInvitation{}, fmt.Errorf("%w: invitation ttl %s exceeds the %s limit",
			protocol.ErrInvalid, ttl, MaxInvitationTTL)
	}
	if maxUses == 0 {
		maxUses = 1
	}

	raw := make([]byte, invitationBytes)
	if _, err := rand.Read(raw); err != nil {
		return protocol.PairInvitation{}, fmt.Errorf("generate invitation token: %w", err)
	}
	token := hex.EncodeToString(raw)

	now := s.now().UTC()
	record := invitationRecord{
		Hash:      hashToken(token),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		MaxUses:   maxUses,
		UseCount:  0,
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return protocol.PairInvitation{}, fmt.Errorf("encode invitation: %w", err)
	}
	if err := writeInvitationFile(s.path(record.Hash, pendingSuffix), append(encoded, '\n')); err != nil {
		return protocol.PairInvitation{}, err
	}

	// Expired leftovers are only cleaned up when the store is used, so a
	// gateway that is never asked for an invitation does not grow either.
	s.pruneExpired(now)

	return protocol.PairInvitation{
		Token:             token,
		ExpiresAt:         record.ExpiresAt,
		ServerFingerprint: serverFingerprint,
		MaxUses:           record.MaxUses,
		UseCount:          record.UseCount,
	}, nil
}

// Consume marks an invitation used. An unknown, expired or already used token
// is refused with protocol.ErrUnauthorized and no further detail: the caller of
// a pairing endpoint is unauthenticated and must not learn which case it hit.
func (s *InvitationStore) Consume(token string) error {
	if token == "" {
		return fmt.Errorf("%w: missing invitation token", protocol.ErrUnauthorized)
	}

	hash := hashToken(token)
	pending := s.path(hash, pendingSuffix)
	used := s.path(hash, usedSuffix)

	// An existing used-file means this token was already spent, on this host or
	// another one sharing the directory.
	if _, err := os.Stat(used); err == nil {
		return fmt.Errorf("%w: invitation has already been used", protocol.ErrUnauthorized)
	}

	raw, err := os.ReadFile(pending)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: unknown invitation token", protocol.ErrUnauthorized)
		}
		return fmt.Errorf("read invitation: %w", err)
	}

	var record invitationRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return fmt.Errorf("parse invitation: %w", err)
	}
	now := s.now().UTC()
	if now.After(record.ExpiresAt) {
		_ = os.Remove(pending)
		return fmt.Errorf("%w: invitation expired at %s", protocol.ErrUnauthorized, record.ExpiresAt.Format(time.RFC3339))
	}

	record.UseCount++
	record.UsedAt = now

	// Single-use limit (default: MaxUses <= 1)
	if record.MaxUses <= 1 && record.MaxUses != -1 {
		if err := os.Rename(pending, used); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: invitation has already been used", protocol.ErrUnauthorized)
			}
			return fmt.Errorf("consume invitation: %w", err)
		}
		if encoded, err := json.MarshalIndent(record, "", "  "); err == nil {
			_ = writeInvitationFile(used, append(encoded, '\n'))
		}
		return nil
	}

	// Multi-use: unlimited within TTL (MaxUses == -1)
	if record.MaxUses == -1 {
		if encoded, err := json.MarshalIndent(record, "", "  "); err == nil {
			_ = writeInvitationFile(pending, append(encoded, '\n'))
		}
		return nil
	}

	// Multi-use with bounded count (MaxUses > 1)
	if record.UseCount < record.MaxUses {
		if encoded, err := json.MarshalIndent(record, "", "  "); err == nil {
			_ = writeInvitationFile(pending, append(encoded, '\n'))
		}
		return nil
	}

	// Reached max uses limit, retire to used
	if err := os.Rename(pending, used); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: invitation has already been used", protocol.ErrUnauthorized)
		}
		return fmt.Errorf("consume invitation: %w", err)
	}
	if encoded, err := json.MarshalIndent(record, "", "  "); err == nil {
		_ = writeInvitationFile(used, append(encoded, '\n'))
	}
	return nil
}

// Pending counts invitations that are still unused and unexpired.
func (s *InvitationStore) Pending() (int, error) {
	now := s.now().UTC()
	records, err := s.records()
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, record := range records {
		if record.pending && record.UsedAt.IsZero() && now.Before(record.ExpiresAt) {
			pending++
		}
	}
	return pending, nil
}

// Used reports how many invitations have been spent, for an operator summary.
func (s *InvitationStore) Used() (int, error) {
	records, err := s.records()
	if err != nil {
		return 0, err
	}
	used := 0
	for _, record := range records {
		if !record.pending || !record.UsedAt.IsZero() {
			used++
		}
	}
	return used, nil
}

// records reads every stored invitation, pending or spent.
func (s *InvitationStore) records() ([]invitationRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read invitation directory: %w", err)
	}

	var records []invitationRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), pendingSuffix) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read invitation %s: %w", entry.Name(), err)
		}
		var record invitationRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			// A partial file is written atomically, so this means corruption
			// rather than an interrupted mint.
			return nil, fmt.Errorf("parse invitation %s: %w", entry.Name(), err)
		}
		record.pending = isPendingFile(entry.Name())
		records = append(records, record)
	}
	return records, nil
}

// pruneExpired drops pending records whose deadline has passed. It is best
// effort by design: housekeeping must never fail the operation that triggered
// it, and a leftover file is harmless because Consume checks the deadline too.
func (s *InvitationStore) pruneExpired(now time.Time) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !isPendingFile(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		var record invitationRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			continue
		}
		if now.After(record.ExpiresAt) {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
}

func (s *InvitationStore) path(hash, suffix string) string {
	return filepath.Join(s.dir, hash+suffix)
}

// isPendingFile distinguishes a spendable record from the record of an already
// spent one: both end in ".json", and only the suffix before it tells them
// apart.
func isPendingFile(name string) bool {
	return strings.HasSuffix(name, pendingSuffix) && !strings.HasSuffix(name, usedSuffix)
}

// IssueInvitation mints an invitation for an existing gateway data directory
// without loading the CA private key, so a gateway that is already running can
// hand out another invitation.
func IssueInvitation(dataDir string, ttl time.Duration, now func() time.Time) (protocol.PairInvitation, error) {
	fingerprint, err := dataDirFingerprint(dataDir)
	if err != nil {
		return protocol.PairInvitation{}, err
	}
	store, err := OpenInvitationStore(InvitationDir(dataDir))
	if err != nil {
		return protocol.PairInvitation{}, err
	}
	if now != nil {
		store.now = now
	}
	return store.Issue(ttl, fingerprint)
}

// PendingInvitations counts the usable invitations in a gateway data directory.
func PendingInvitations(dataDir string) (int, error) {
	store, err := OpenInvitationStore(InvitationDir(dataDir))
	if err != nil {
		return 0, err
	}
	return store.Pending()
}

// InvitationSummary reports how many invitations a gateway directory holds.
func InvitationSummary(dataDir string) (pending, used int, err error) {
	store, err := OpenInvitationStore(InvitationDir(dataDir))
	if err != nil {
		return 0, 0, err
	}
	if pending, err = store.Pending(); err != nil {
		return 0, 0, err
	}
	if used, err = store.Used(); err != nil {
		return 0, 0, err
	}
	return pending, used, nil
}

// InvitationDir names the invitation directory inside a gateway data
// directory.
func InvitationDir(dataDir string) string {
	return filepath.Join(dataDir, invitationDirName)
}

// dataDirFingerprint reads the CA certificate an operator already trusts and
// returns its SHA-256 fingerprint. The CA key is deliberately not read.
func dataDirFingerprint(dataDir string) (string, error) {
	certPEM, err := os.ReadFile(filepath.Join(dataDir, caCertFilename))
	if err != nil {
		return "", fmt.Errorf("%w: %s does not look like a gateway data directory: %v",
			protocol.ErrNotFound, dataDir, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("%w: %s is not a valid certificate", protocol.ErrInvalid, caCertFilename)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("%w: parse CA certificate: %v", protocol.ErrInvalid, err)
	}
	return fingerprintOf(cert), nil
}

// fingerprintOf returns the hex SHA-256 of a certificate.
func fingerprintOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// writeInvitationFile publishes a private file atomically: the data reaches the
// disk before the rename, so a crash cannot leave a half-written record under
// its final name.
func writeInvitationFile(path string, data []byte) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("flush %s: %w", tmp, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return nil
}
