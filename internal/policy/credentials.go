package policy

import (
	"context"
	"database/sql"
	"time"
)

// CredentialInfo deliberately excludes token/hash and carries lifecycle metadata.
type CredentialInfo struct {
	ID        string     `json:"id"`
	Role      Role       `json:"role"`
	IssuedAt  time.Time  `json:"issued_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Credentials returns an ID-cursor page, bounded even for large stores.
func (s *Store) Credentials(ctx context.Context, after string, limit int) ([]CredentialInfo, error) {
	if limit < 1 || limit > 100 || len(after) > maxIDBytes {
		return nil, invalidf("invalid credential page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,role,issued_at,expires_at,revoked_at FROM principals WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, dbError("list credentials", err)
	}
	defer rows.Close()
	out := make([]CredentialInfo, 0)
	for rows.Next() {
		var item CredentialInfo
		var issued, expires string
		var revoked sql.NullString
		if err := rows.Scan(&item.ID, &item.Role, &issued, &expires, &revoked); err != nil {
			return nil, err
		}
		if item.IssuedAt, err = parseTime(issued); err != nil {
			return nil, err
		}
		if item.ExpiresAt, err = parseTime(expires); err != nil {
			return nil, err
		}
		if revoked.Valid {
			v, err := parseTime(revoked.String)
			if err != nil {
				return nil, err
			}
			item.RevokedAt = &v
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// LimitLifetime shortens existing credentials on upgrade; never extends or revives them.
func (s *Store) LimitLifetime(ctx context.Context, lifetime time.Duration) error {
	if lifetime <= 0 || lifetime > maxValidity {
		return invalidf("invalid lifetime")
	}
	return s.writeTx(ctx, func(q querier, now time.Time) error {
		after := ""
		for {
			rows, err := q.QueryContext(ctx, `SELECT id,issued_at FROM principals WHERE revoked_at IS NULL AND id > ? ORDER BY id LIMIT 100`, after)
			if err != nil {
				return err
			}
			type item struct{ id, expiry string }
			changes := make([]item, 0, 100)
			for rows.Next() {
				var id, issued string
				if err := rows.Scan(&id, &issued); err != nil {
					rows.Close()
					return err
				}
				ts, err := parseTime(issued)
				if err != nil {
					rows.Close()
					return err
				}
				changes = append(changes, item{id, formatTime(ts.Add(lifetime))})
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, c := range changes {
				if _, err := q.ExecContext(ctx, `UPDATE principals SET expires_at = ? WHERE id = ? AND expires_at > ?`, c.expiry, c.id, c.expiry); err != nil {
					return err
				}
			}
			if len(changes) < 100 {
				return nil
			}
			after = changes[len(changes)-1].id
		}
	})
}
