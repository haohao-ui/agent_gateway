// Package devicestore persists certificate bindings and device revocations.
package devicestore

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"agent-gateway/internal/protocol"
	"encoding/json"
	"modernc.org/sqlite"
)

// Store checks the database on every authorization; it never caches revocations.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens a dedicated device database. Parent directories must already exist.
// File mode restricts newly created files on Unix; Windows ACLs remain the caller's responsibility.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" || strings.HasPrefix(path, "file:") || strings.ContainsAny(path, "?#") {
		return nil, protocol.ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("device database path: %w", err)
	}
	fi, err := os.Stat(absolute)
	if err == nil && fi.IsDir() {
		return nil, protocol.ErrInvalid
	}
	f, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open device database: %w", err)
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	q := u.Query()
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "FULL")
	q.Set("_foreign_keys", "on")
	q.Set("_busy_timeout", "200")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = s.write(ctx, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version > 1 {
			return fmt.Errorf("unsupported device schema: %w", protocol.ErrConflict)
		}
		if version == 0 {
			_, err := tx.ExecContext(ctx, `CREATE TABLE devices (
 node_id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, expires_at TEXT NOT NULL,
 revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)),
 machine_id TEXT, hostname TEXT, allowed_tools TEXT);
 CREATE TABLE device_audit (
 id INTEGER PRIMARY KEY, actor TEXT NOT NULL, action TEXT NOT NULL,
 node_id TEXT NOT NULL REFERENCES devices(node_id), outcome TEXT NOT NULL,
 reason TEXT NOT NULL, occurred_at TEXT NOT NULL);
 PRAGMA user_version=1;`)
			return err
		}
		// If existing version 1 database lacks the columns, add them idempotently.
		_, _ = tx.ExecContext(ctx, "ALTER TABLE devices ADD COLUMN machine_id TEXT;")
		_, _ = tx.ExecContext(ctx, "ALTER TABLE devices ADD COLUMN hostname TEXT;")
		_, _ = tx.ExecContext(ctx, "ALTER TABLE devices ADD COLUMN allowed_tools TEXT;")
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate devices: %w", err)
	}
	return s, nil
}

// Close releases database resources.
func (s *Store) Close() error { return s.db.Close() }

func validText(v string, max int) bool {
	return len(v) <= max && strings.TrimSpace(v) != "" && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}
func validFingerprint(v string) bool {
	if len(v) != 64 || strings.ToLower(v) != v {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

const timeFormat = "2006-01-02T15:04:05.000000000Z"

// Register creates an immutable certificate binding. An identical active binding
// is idempotent; changed or revoked bindings must never be silently replaced.
func (s *Store) Register(ctx context.Context, nodeID, fingerprint string, expiresAt time.Time) error {
	return s.RegisterWithDevice(ctx, nodeID, fingerprint, "", "", expiresAt)
}

// RegisterWithDevice creates a certificate binding associated with a machine identity.
// If another active binding belongs to the same physical machine (machine_id or hostname),
// it is atomically superseded so a re-enrolled host does not leave ghost nodes.
func (s *Store) RegisterWithDevice(ctx context.Context, nodeID, fingerprint, machineID, hostname string, expiresAt time.Time) error {
	if !validText(nodeID, 128) || !validFingerprint(fingerprint) || expiresAt.Year() < 1 || expiresAt.Year() > 9999 {
		return protocol.ErrInvalid
	}
	expiry := expiresAt.UTC().Format(timeFormat)
	return s.write(ctx, func(tx *sql.Tx) error {
		if !expiresAt.After(s.now()) {
			return protocol.ErrInvalid
		}
		var oldFingerprint, oldExpiry string
		var revoked bool
		err := tx.QueryRowContext(ctx, "SELECT fingerprint, expires_at, revoked FROM devices WHERE node_id=?", nodeID).Scan(&oldFingerprint, &oldExpiry, &revoked)
		if err == nil {
			if !revoked && oldFingerprint == fingerprint && oldExpiry == expiry {
				return nil
			}
			return protocol.ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		cleanMachine := strings.TrimSpace(machineID)
		cleanHost := strings.TrimSpace(hostname)
		if cleanMachine != "" || cleanHost != "" {
			var query string
			var args []any
			if cleanMachine != "" && cleanHost != "" {
				query = "SELECT node_id FROM devices WHERE revoked=0 AND node_id!=? AND (machine_id=? OR (hostname=? AND hostname!=''))"
				args = []any{nodeID, cleanMachine, cleanHost}
			} else if cleanMachine != "" {
				query = "SELECT node_id FROM devices WHERE revoked=0 AND node_id!=? AND machine_id=?"
				args = []any{nodeID, cleanMachine}
			} else {
				query = "SELECT node_id FROM devices WHERE revoked=0 AND node_id!=? AND hostname=? AND hostname!=''"
				args = []any{nodeID, cleanHost}
			}
			rows, qErr := tx.QueryContext(ctx, query, args...)
			if qErr == nil {
				var supersedes []string
				for rows.Next() {
					var oldID string
					if scanErr := rows.Scan(&oldID); scanErr == nil {
						supersedes = append(supersedes, oldID)
					}
				}
				rows.Close()
				for _, oldID := range supersedes {
					_, _ = tx.ExecContext(ctx, "UPDATE devices SET revoked=1 WHERE node_id=?", oldID)
					_, _ = tx.ExecContext(ctx, "INSERT INTO device_audit(actor,action,node_id,outcome,reason,occurred_at) VALUES(?,?,?,?,?,?)",
						"gateway", "device.supersede", oldID, "success", "superseded_by:"+nodeID, s.now().UTC().Format(timeFormat))
				}
			}
		}

		_, err = tx.ExecContext(ctx, "INSERT INTO devices(node_id,fingerprint,expires_at,machine_id,hostname) VALUES(?,?,?,?,?)", nodeID, fingerprint, expiry, cleanMachine, cleanHost)
		return err
	})
}

// Authorize requires the exact unexpired, unrevoked persisted certificate binding.
// Unknown, malformed, revoked, expired and mismatched credentials fail identically.
func (s *Store) Authorize(ctx context.Context, nodeID, fingerprint string) error {
	if !validText(nodeID, 128) || !validFingerprint(fingerprint) {
		return protocol.ErrUnauthorized
	}
	var stored, expiry string
	var revoked bool
	err := s.db.QueryRowContext(ctx, "SELECT fingerprint, expires_at, revoked FROM devices WHERE node_id=?", nodeID).Scan(&stored, &expiry, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.ErrUnauthorized
	}
	if err != nil {
		return fmt.Errorf("authorize device: %w", err)
	}
	until, err := time.Parse(timeFormat, expiry)
	if err != nil || revoked || !until.After(s.now()) || subtle.ConstantTimeCompare([]byte(stored), []byte(fingerprint)) != 1 {
		return protocol.ErrUnauthorized
	}
	return nil
}

// Revoke atomically revokes a device and records the actor and reason. A repeated
// revocation succeeds without replacing the original audit attribution.
func (s *Store) Revoke(ctx context.Context, nodeID, actor, reason string) error {
	if !validText(nodeID, 128) || !validText(actor, 128) || !validText(reason, 1024) {
		return protocol.ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var revoked bool
		err := tx.QueryRowContext(ctx, "SELECT revoked FROM devices WHERE node_id=?", nodeID).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.ErrNotFound
		}
		if err != nil {
			return err
		}
		if revoked {
			return nil
		}
		if _, err = tx.ExecContext(ctx, "UPDATE devices SET revoked=1 WHERE node_id=?", nodeID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO device_audit(actor,action,node_id,outcome,reason,occurred_at) VALUES(?,?,?,?,?,?)", actor, "device.revoke", nodeID, "success", reason, s.now().UTC().Format(timeFormat))
		return err
	})
}

// Delete completely removes a registered device and its audit history from the store.
func (s *Store) Delete(ctx context.Context, nodeID string) error {
	if !validText(nodeID, 128) {
		return protocol.ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var exists bool
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE node_id=?", nodeID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM device_audit WHERE node_id=?", nodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM devices WHERE node_id=?", nodeID); err != nil {
			return err
		}
		return nil
	})
}

// Device represents a registered node certificate binding and its status.
type Device struct {
	NodeID       string    `json:"node_id"`
	Fingerprint  string    `json:"fingerprint"`
	ExpiresAt    time.Time `json:"expires_at"`
	Revoked      bool      `json:"revoked"`
	MachineID    string    `json:"machine_id,omitempty"`
	Hostname     string    `json:"hostname,omitempty"`
	AllowedTools []string  `json:"allowed_tools,omitempty"`
}

// List returns all registered devices in the database ordered by node_id.
func (s *Store) List(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT node_id, fingerprint, expires_at, revoked, COALESCE(machine_id, ''), COALESCE(hostname, ''), COALESCE(allowed_tools, '') FROM devices ORDER BY node_id ASC")
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var d Device
		var expiry string
		var revoked int
		var machID, host, rawTools sql.NullString
		if err := rows.Scan(&d.NodeID, &d.Fingerprint, &expiry, &revoked, &machID, &host, &rawTools); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		until, err := time.Parse(timeFormat, expiry)
		if err != nil {
			return nil, fmt.Errorf("parse device expiry: %w", err)
		}
		d.ExpiresAt = until
		d.Revoked = revoked == 1
		if machID.Valid {
			d.MachineID = machID.String
		}
		if host.Valid {
			d.Hostname = host.String
		}
		if rawTools.Valid && strings.TrimSpace(rawTools.String) != "" {
			_ = json.Unmarshal([]byte(rawTools.String), &d.AllowedTools)
		}
		if d.AllowedTools == nil {
			d.AllowedTools = []string{}
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	if devices == nil {
		devices = []Device{}
	}
	return devices, nil
}

// GetAllowedTools returns the list of authorized tools for a node.
func (s *Store) GetAllowedTools(ctx context.Context, nodeID string) ([]string, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT allowed_tools FROM devices WHERE node_id=?", nodeID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, protocol.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get allowed tools: %w", err)
	}
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return []string{}, nil
	}
	var tools []string
	if err := json.Unmarshal([]byte(raw.String), &tools); err != nil {
		return []string{}, nil
	}
	if tools == nil {
		tools = []string{}
	}
	return tools, nil
}

// SetAllowedTools updates the authorized tools list for a node.
func (s *Store) SetAllowedTools(ctx context.Context, nodeID string, tools []string) error {
	if !validText(nodeID, 128) {
		return protocol.ErrInvalid
	}
	if tools == nil {
		tools = []string{}
	}
	bytes, err := json.Marshal(tools)
	if err != nil {
		return protocol.ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var exists bool
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE node_id=?", nodeID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE devices SET allowed_tools=? WHERE node_id=?", string(bytes), nodeID)
		return err
	})
}

// SQLite's busy handler may not immediately observe cancellation; each wait is
// bounded to 200ms, with context-aware retries and an overall five-second cap.
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	deadline := time.Now().Add(5 * time.Second)
	var tx *sql.Tx
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		tx, err = s.db.BeginTx(ctx, nil)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || (sqliteErr.Code()&255 != 5 && sqliteErr.Code()&255 != 6) {
			return fmt.Errorf("begin device transaction: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device database busy: %w", protocol.ErrConflict)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
