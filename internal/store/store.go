// Package store keeps agyrelay's mutable state in one SQLite database:
// settings, accounts, API keys, admin sessions, request records, quota
// samples, and the audit log.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// migrations[i] moves the schema from version i to i+1.
var migrations = []string{
	`CREATE TABLE settings (
		name  TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE accounts (
		name       TEXT PRIMARY KEY,
		env        TEXT NOT NULL,
		paused     INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE api_keys (
		id         INTEGER PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		sha256     BLOB NOT NULL UNIQUE,
		note       TEXT NOT NULL,
		models     TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		revoked_at INTEGER
	);
	CREATE TABLE admin_sessions (
		token_hash BLOB PRIMARY KEY,
		public_id  TEXT NOT NULL UNIQUE,
		csrf       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		seen_at    INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		ip         TEXT NOT NULL,
		user_agent TEXT NOT NULL
	);
	CREATE TABLE audit (
		id     INTEGER PRIMARY KEY,
		at     INTEGER NOT NULL,
		ip     TEXT NOT NULL,
		action TEXT NOT NULL,
		detail TEXT NOT NULL
	);
	CREATE INDEX audit_at ON audit (at);
	CREATE TABLE quota_samples (
		account   TEXT NOT NULL,
		at        INTEGER NOT NULL,
		bucket    TEXT NOT NULL,
		grp       TEXT NOT NULL,
		win       TEXT NOT NULL,
		remaining REAL NOT NULL,
		reset_at  INTEGER NOT NULL,
		PRIMARY KEY (account, bucket, at)
	);
	CREATE TABLE requests (
		id          INTEGER PRIMARY KEY,
		at          INTEGER NOT NULL,
		key_name    TEXT NOT NULL,
		account     TEXT NOT NULL,
		model       TEXT NOT NULL,
		slug        TEXT NOT NULL,
		api         TEXT NOT NULL,
		stream      INTEGER NOT NULL,
		mode        TEXT NOT NULL,
		status      TEXT NOT NULL,
		error       TEXT NOT NULL,
		finish      TEXT NOT NULL,
		duration_ms INTEGER NOT NULL,
		ttft_ms     INTEGER,
		input       INTEGER NOT NULL,
		cached      INTEGER NOT NULL,
		output      INTEGER NOT NULL,
		reasoning   INTEGER NOT NULL,
		cost_usd    REAL,
		quota_group TEXT NOT NULL,
		quota_used  REAL
	);
	CREATE INDEX requests_at ON requests (at);
	CREATE INDEX requests_key_at ON requests (key_name, at);
	CREATE INDEX requests_account_at ON requests (account, quota_group, at);`,

	`ALTER TABLE accounts ADD COLUMN proxy TEXT NOT NULL DEFAULT '';
	ALTER TABLE api_keys ADD COLUMN limits TEXT NOT NULL DEFAULT '{}';
	CREATE TABLE quota_cursors (
		account TEXT NOT NULL,
		bucket  TEXT NOT NULL,
		at      INTEGER NOT NULL,
		PRIMARY KEY (account, bucket)
	);`,
}

// ErrNotFound means the named row does not exist.
var ErrNotFound = errors.New("not found")

// ErrExists means a row with the same unique name exists.
var ErrExists = errors.New("already exists")

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database and migrates its schema.
func Open(path string) (*Store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serializes writers; the load is a few writes per request.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this agyrelay (%d)", version, len(migrations))
	}
	for v := version; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Setting decodes the named setting into v; it reports false when unset.
func (s *Store) Setting(ctx context.Context, name string, v any) (bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, name).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return false, fmt.Errorf("setting %s: %w", name, err)
	}
	return true, nil
}

func (s *Store) SetSetting(ctx context.Context, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, string(raw))
	return err
}
