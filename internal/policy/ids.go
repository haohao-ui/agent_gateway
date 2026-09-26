package policy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Identifier and secret generation.
//
// Principal IDs are always generated here with crypto/rand and never accepted
// from a caller, so an operator cannot choose an identifier it can predict or
// reuse. Tokens carry 256 bits of randomness, which is why the stored hash
// needs no salt or key derivation: there is no low-entropy input for an
// attacker to guess or precompute, and the hash is not a password hash because
// it is not protecting a guessable secret.
const (
	principalIDPrefix = "prn_"
	idRandomBytes     = 16
	tokenRandomBytes  = 32
)

// newID returns a random identifier with the given prefix: 128 random bits as
// lowercase hex.
func newID(prefix string) (string, error) {
	var buf [idRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("policy: generate id: %w", err)
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}

// newToken returns a fresh bearer secret. It is returned to the caller once
// and only its hash is stored.
func newToken() (string, error) {
	var buf [tokenRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("policy: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// tokenHash is the persisted form of a token.
//
// Authenticate looks a credential up by this value, so the comparison that
// decides whether a token exists happens inside SQLite's B-tree rather than in
// a byte-by-byte loop here. That is safe for this credential and would not be
// for a password: the preimage is 256 bits of randomness, so an attacker
// cannot construct a near-miss hash to walk the index with, and there is no
// partial match to learn anything from. A leak of the database therefore does
// not yield a usable token, because it yields the thing a token is hashed
// into and SHA-256 is not invertible.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
