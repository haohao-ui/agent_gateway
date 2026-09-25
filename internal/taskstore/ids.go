package taskstore

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Identifier and secret generation.
//
// Task IDs, attempt IDs and lease tokens are always generated here with
// crypto/rand and never accepted from a caller, so a client cannot choose an
// identifier it can predict or reuse. Tokens are 256 bits of randomness, which
// is why the stored hash needs no salt or key derivation: there is no
// low-entropy input for an attacker to guess or precompute.
const (
	taskIDPrefix     = "task_"
	attemptIDPrefix  = "att_"
	idRandomBytes    = 16
	tokenRandomBytes = 32
)

// newID returns a random identifier with the given prefix: 128 random bits as
// lowercase hex.
func newID(prefix string) (string, error) {
	var buf [idRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("taskstore: generate id: %w", err)
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}

// newToken returns a fresh lease secret. It is returned to the claimant once
// and only its hash is stored.
func newToken() (string, error) {
	var buf [tokenRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("taskstore: generate lease token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// tokenHash is the persisted form of a lease token.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// tokenMatches reports whether token hashes to storedHash. The comparison is
// constant time so that a caller cannot recover a token byte by byte from
// timing, and a stored hash that is not valid hex simply never matches.
func tokenMatches(storedHash, token string) bool {
	want, err := hex.DecodeString(storedHash)
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(want, got[:]) == 1
}
