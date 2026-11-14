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
	"strings"

	_ "modernc.org/sqlite"
)

// migrations[i] moves the schema from version i to i+1.
var migrations = []string{
	// api_keys.hash is the key's lowercase hex SHA-256 and identifies it;
	// requests keep the key's name for display after the key is deleted.
	// key_usage keeps each key's list-price spend after old requests are
	// pruned; key limits count it. key_totals likewise keeps each key's
	// successful requests and tokens by model.
	`CREATE TABLE settings (
		name  TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE accounts (
		name       TEXT PRIMARY KEY,
		env        TEXT NOT NULL DEFAULT '[]',
		proxy      TEXT NOT NULL DEFAULT '',
		paused     INTEGER NOT NULL DEFAULT 0,
		priority   INTEGER NOT NULL DEFAULT 0,
		models     TEXT NOT NULL DEFAULT '[]',
		created_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE api_keys (
		hash        TEXT PRIMARY KEY,
		name        TEXT NOT NULL UNIQUE,
		note        TEXT NOT NULL DEFAULT '',
		models      TEXT NOT NULL DEFAULT '[]',
		max_workers INTEGER,
		limit_total REAL,
		limits      TEXT NOT NULL DEFAULT '{}',
		created_at  INTEGER NOT NULL DEFAULT 0,
		revoked_at  INTEGER
	);
	CREATE TABLE admin_sessions (
		token_hash BLOB PRIMARY KEY,
		public_id  TEXT NOT NULL UNIQUE,
		csrf       TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0,
		seen_at    INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER NOT NULL DEFAULT 0,
		ip         TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE audit (
		id     INTEGER PRIMARY KEY,
		at     INTEGER NOT NULL DEFAULT 0,
		ip     TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX audit_at ON audit (at);
	CREATE TABLE quota_samples (
		account   TEXT NOT NULL,
		at        INTEGER NOT NULL,
		bucket    TEXT NOT NULL,
		grp       TEXT NOT NULL DEFAULT '',
		win       TEXT NOT NULL DEFAULT '',
		remaining REAL NOT NULL DEFAULT 0,
		reset_at  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account, bucket, at)
	);
	CREATE TABLE quota_cursors (
		account TEXT NOT NULL,
		bucket  TEXT NOT NULL,
		at      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account, bucket)
	);
	CREATE TABLE requests (
		id            INTEGER PRIMARY KEY,
		at            INTEGER NOT NULL DEFAULT 0,
		key_hash      TEXT NOT NULL DEFAULT '',
		key_name      TEXT NOT NULL DEFAULT '',
		account       TEXT NOT NULL DEFAULT '',
		model         TEXT NOT NULL DEFAULT '',
		slug          TEXT NOT NULL DEFAULT '',
		effort        TEXT NOT NULL DEFAULT '',
		api           TEXT NOT NULL DEFAULT '',
		stream        INTEGER NOT NULL DEFAULT 0,
		mode          TEXT NOT NULL DEFAULT '',
		replay_reason TEXT NOT NULL DEFAULT '',
		status        TEXT NOT NULL DEFAULT '',
		error         TEXT NOT NULL DEFAULT '',
		finish        TEXT NOT NULL DEFAULT '',
		duration_ms   INTEGER NOT NULL DEFAULT 0,
		ttft_ms       INTEGER,
		input         INTEGER NOT NULL DEFAULT 0,
		cached        INTEGER NOT NULL DEFAULT 0,
		output        INTEGER NOT NULL DEFAULT 0,
		reasoning     INTEGER NOT NULL DEFAULT 0,
		cost_usd      REAL,
		quota_group   TEXT NOT NULL DEFAULT '',
		quota_used    REAL,
		quota_skip    TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX requests_at ON requests (at);
	CREATE INDEX requests_key_at ON requests (key_hash, at);
	CREATE INDEX requests_account_at ON requests (account, quota_group, at);
	CREATE TABLE key_usage (
		key_hash TEXT NOT NULL,
		grp      TEXT NOT NULL,
		cost_usd REAL NOT NULL DEFAULT 0,
		PRIMARY KEY (key_hash, grp)
	);
	CREATE TABLE key_totals (
		key_hash TEXT NOT NULL,
		model    TEXT NOT NULL,
		requests INTEGER NOT NULL DEFAULT 0,
		input    INTEGER NOT NULL DEFAULT 0,
		output   INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (key_hash, model)
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for i, m := range migrations {
		if err := applySchema(tx, m); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	for _, initSQL := range []string{
		`UPDATE accounts SET env = '[]' WHERE env = ''`,
		`UPDATE accounts SET models = '[]' WHERE models = ''`,
		`UPDATE api_keys SET models = '[]' WHERE models = ''`,
		`UPDATE api_keys SET limits = '{}' WHERE limits = ''`,
	} {
		if _, err := tx.Exec(initSQL); err != nil {
			return err
		}
	}
	if version < len(migrations) {
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations))); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// applySchema creates missing tables, adds missing columns with their
// defaults, and creates missing indexes so existing databases pick up new
// state columns and tables on startup.
func applySchema(tx *sql.Tx, schema string) error {
	var indexes []string
	for _, raw := range strings.Split(schema, ";") {
		stmt := strings.TrimSpace(raw)
		if stmt == "" {
			continue
		}
		if strings.HasPrefix(stmt, "CREATE TABLE ") {
			if err := applyTable(tx, stmt); err != nil {
				return err
			}
			continue
		}
		if rest, ok := strings.CutPrefix(stmt, "CREATE INDEX "); ok {
			indexes = append(indexes, "CREATE INDEX IF NOT EXISTS "+strings.TrimSpace(rest))
			continue
		}
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	for _, idx := range indexes {
		if _, err := tx.Exec(idx); err != nil {
			return err
		}
	}
	return nil
}

func applyTable(tx *sql.Tx, stmt string) error {
	rest := strings.TrimSpace(strings.TrimPrefix(stmt, "CREATE TABLE "))
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS " + rest); err != nil {
		return err
	}
	head, body, ok := strings.Cut(rest, "(")
	if !ok {
		return nil
	}
	table := strings.TrimSpace(head)
	body = strings.TrimSuffix(strings.TrimSpace(body), ")")
	have, err := tableColumns(tx, table)
	if err != nil {
		return err
	}
	for _, clause := range splitClauses(body) {
		fields := strings.Fields(clause)
		if len(fields) == 0 || isTableConstraint(fields[0]) || have[fields[0]] {
			continue
		}
		if _, err := tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + clause); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, fields[0], err)
		}
	}
	return nil
}

func tableColumns(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

func splitClauses(body string) []string {
	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\'':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
			}
		case ',':
			if !inQuote && depth == 0 {
				if c := strings.TrimSpace(body[start:i]); c != "" {
					out = append(out, c)
				}
				start = i + 1
			}
		}
	}
	if c := strings.TrimSpace(body[start:]); c != "" {
		out = append(out, c)
	}
	return out
}

func isTableConstraint(word string) bool {
	switch strings.ToUpper(word) {
	case "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
		return true
	default:
		return false
	}
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
