package policy

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Input limits from the task book.
const (
	// maxScopeNodes bounds a node scope. The limit is checked on the entries
	// the caller passed, before duplicates are removed, so a caller cannot
	// present a list longer than the limit and rely on deduplication to bring
	// it back under.
	maxScopeNodes = 100

	// maxNodeIDBytes bounds one node ID and maxIDBytes one principal ID.
	maxNodeIDBytes = 128
	maxIDBytes     = 128

	// maxTokenBytes bounds an opaque token on the way in. A real token is 43
	// characters of base64url from 32 random bytes; this is only an upper
	// bound so that a huge string is rejected before it is hashed or used as a
	// query parameter.
	maxTokenBytes = 128

	// maxValidity is the longest validity a caller may ask for.
	maxValidity = 365 * 24 * time.Hour
)

// Issue creates a principal with the given role, node scope and expiry, and
// returns it together with a freshly generated token.
//
// The token is the only time the cleartext credential exists outside the
// caller: it is not stored and cannot be recovered, so a lost token is
// replaced by issuing a new principal rather than looked up. The principal ID
// is generated here as well; a caller never chooses an identifier.
//
// Inputs are validated before the write lock is taken, except for the expiry
// window, which is measured from the moment the write lock was acquired: a
// credential issued around a contested lock must still be valid for the whole
// period the caller asked for, and must not be stamped against a time that has
// already passed.
//
// Issue is an administrative operation for trusted local callers. It performs
// no authentication of its own — the caller's authority to create a principal
// is established above this package, by whatever path the coordinator wires to
// it.
func (s *Store) Issue(ctx context.Context, role Role, nodeIDs []string, expiresAt time.Time) (Principal, string, error) {
	scope, err := validateScope(role, nodeIDs)
	if err != nil {
		return Principal{}, "", err
	}
	token, err := newToken()
	if err != nil {
		return Principal{}, "", err
	}
	id, err := newID(principalIDPrefix)
	if err != nil {
		return Principal{}, "", err
	}
	p := Principal{ID: id, Role: role, NodeIDs: scope}
	err = s.writeTx(ctx, func(q querier, now time.Time) error {
		if err := validateExpiry(expiresAt, now); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO principals (id, role, token_hash, issued_at, expires_at, revoked_at)
			 VALUES (?, ?, ?, ?, ?, NULL)`,
			p.ID, string(p.Role), tokenHash(token), formatTime(now), formatTime(expiresAt)); err != nil {
			return dbError("record principal", err)
		}
		for _, nodeID := range scope {
			if _, err := q.ExecContext(ctx,
				`INSERT INTO principal_scopes (principal_id, node_id) VALUES (?, ?)`,
				p.ID, nodeID); err != nil {
				return dbError("record principal scope", err)
			}
		}
		return nil
	})
	if err != nil {
		return Principal{}, "", err
	}
	return p, token, nil
}

// Authenticate resolves a token to the principal it belongs to.
//
// It fails with protocol.ErrUnauthorized when the token is unknown, its
// principal was revoked, or it has expired, and the three are deliberately
// indistinguishable: the same error and the same message, so the result of a
// call cannot be used to probe which credentials exist or which of them were
// revoked. The reason is discoverable locally, from the row's revoked_at and
// expires_at, which is where an operator diagnoses it.
//
// An accepted token returns the scope as it is stored, inside one read
// snapshot so that a revocation committed concurrently cannot produce a
// principal assembled from two versions of the database. The verdict is a
// point-in-time answer by nature: a credential revoked a moment after it was
// verified was verified while it was still live.
func (s *Store) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token == "" || len(token) > maxTokenBytes {
		return Principal{}, unauthorizedTokenf()
	}
	hash := tokenHash(token)
	var p Principal
	err := s.readTx(ctx, func(q querier) error {
		var (
			role      string
			expiresAt string
			revokedAt sql.NullString
		)
		err := q.QueryRowContext(ctx,
			`SELECT id, role, expires_at, revoked_at FROM principals WHERE token_hash = ?`,
			hash).Scan(&p.ID, &role, &expiresAt, &revokedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return unauthorizedTokenf()
		}
		if err != nil {
			return dbError("read principal", err)
		}
		if revokedAt.Valid {
			return unauthorizedTokenf()
		}
		expires, err := parseTime(expiresAt)
		if err != nil {
			return dbError("read principal expiry", err)
		}
		if !expires.After(s.now()) {
			return unauthorizedTokenf()
		}
		scope, err := readScope(ctx, q, p.ID)
		if err != nil {
			return err
		}
		p.Role = Role(role)
		p.NodeIDs = scope
		return nil
	})
	if err != nil {
		return Principal{}, err
	}
	return p, nil
}

// Revoke marks a principal as revoked. Revocation takes effect for every
// Store that shares the database as soon as the transaction commits, the
// handle that issued it included, because no handle caches a principal.
//
// The row is kept, not deleted: revoked_at is the record that this credential
// existed and was withdrawn, and deleting it would make "was this revoked, or
// was it never issued?" unanswerable afterwards.
//
// Revoking an already revoked principal succeeds and leaves the original
// timestamp in place, so the call is idempotent and a retry cannot rewrite
// when the revocation happened. An unknown principal is
// protocol.ErrNotFound — the operation cannot be completed, and unlike an
// authentication failure there is nothing here to guess at.
func (s *Store) Revoke(ctx context.Context, principalID string) error {
	if err := validatePrincipalID(principalID); err != nil {
		return err
	}
	return s.writeTx(ctx, func(q querier, now time.Time) error {
		var revokedAt sql.NullString
		err := q.QueryRowContext(ctx, `SELECT revoked_at FROM principals WHERE id = ?`, principalID).Scan(&revokedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return notFoundf("principal does not exist")
		}
		if err != nil {
			return dbError("read principal", err)
		}
		if revokedAt.Valid {
			return nil
		}
		res, err := q.ExecContext(ctx,
			`UPDATE principals SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
			formatTime(now), principalID)
		if err != nil {
			return dbError("revoke principal", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return dbError("revoke principal", err)
		}
		if affected != 1 {
			// Unreachable while the write lock is held; kept as a guard so a
			// future change to the transaction shape cannot report a
			// revocation that did not happen.
			return conflictf("principal changed while being revoked")
		}
		return nil
	})
}

// readScope reads the node scope of a principal in storage order.
func readScope(ctx context.Context, q querier, principalID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT node_id FROM principal_scopes WHERE principal_id = ? ORDER BY node_id`, principalID)
	if err != nil {
		return nil, dbError("read principal scope", err)
	}
	defer rows.Close()
	var scope []string
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, dbError("read principal scope", err)
		}
		scope = append(scope, nodeID)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("read principal scope", err)
	}
	return scope, nil
}

// validateScope checks a node scope against its role and returns the canonical
// form: deduplicated, sorted, and nil for an admin.
//
// The rules are the ones that keep a scope from meaning more than it says.
// An admin must be empty because an admin is global; an operator or a viewer
// must name nodes because an empty scope would otherwise be ambiguous between
// "no nodes" and "every node". A wildcard is refused rather than interpreted:
// node IDs are compared as exact strings, and a node genuinely called "*"
// would be a scope that silently covers everything.
func validateScope(role Role, nodeIDs []string) ([]string, error) {
	switch role {
	case Admin:
		if len(nodeIDs) > 0 {
			return nil, invalidf("admin scope must be empty: an admin is global, not node-scoped")
		}
		return nil, nil
	case Operator, Viewer:
	default:
		return nil, invalidf("role is not a known role")
	}
	if len(nodeIDs) == 0 {
		return nil, invalidf("%s scope must name at least one node", role)
	}
	if len(nodeIDs) > maxScopeNodes {
		return nil, invalidf("scope has %d entries, limit is %d", len(nodeIDs), maxScopeNodes)
	}
	seen := make(map[string]struct{}, len(nodeIDs))
	canonical := make([]string, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if err := validateNodeID(nodeID); err != nil {
			return nil, err
		}
		if _, dup := seen[nodeID]; dup {
			continue
		}
		seen[nodeID] = struct{}{}
		canonical = append(canonical, nodeID)
	}
	sort.Strings(canonical)
	return canonical, nil
}

// validateExpiry checks the requested validity against the instant the write
// lock was taken.
func validateExpiry(expiresAt, now time.Time) error {
	if !expiresAt.After(now) {
		return invalidf("expiry must be in the future")
	}
	if expiresAt.Sub(now) > maxValidity {
		return invalidf("validity exceeds the %d day maximum", int(maxValidity.Hours()/24))
	}
	return nil
}

// validateNodeID checks one node identifier.
func validateNodeID(nodeID string) error {
	if err := validateBoundedText("node_id", nodeID, maxNodeIDBytes); err != nil {
		return err
	}
	if strings.ContainsAny(nodeID, "*?") {
		return invalidf("node_id must not contain a wildcard character")
	}
	return nil
}

// validatePrincipalID checks the identifier a revoke addresses.
func validatePrincipalID(principalID string) error {
	return validateBoundedText("principal_id", principalID, maxIDBytes)
}

// validateBoundedText checks a caller-supplied identifier. Values are never
// echoed in the error: only the field name and its length are reported, so a
// bad request cannot reflect attacker-controlled bytes back into logs.
func validateBoundedText(field, value string, maxBytes int) error {
	if value == "" {
		return invalidf("%s must not be empty", field)
	}
	if len(value) > maxBytes {
		return invalidf("%s is %d bytes, limit is %d", field, len(value), maxBytes)
	}
	if !utf8.ValidString(value) {
		return invalidf("%s must be valid UTF-8", field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return invalidf("%s must not contain control characters", field)
		}
	}
	return nil
}
