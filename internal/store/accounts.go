package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Account is a registered Antigravity account. Its sign-in lives in its
// HOME directory, not here.
type Account struct {
	Name string
	// Env is extra environment for the account's agy (e.g. HTTPS_PROXY).
	Env []string
	// Paused takes the account out of rotation.
	Paused    bool
	CreatedAt time.Time
}

func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, env, paused, created_at FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var env string
		var created int64
		if err := rows.Scan(&a.Name, &env, &a.Paused, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(env), &a.Env); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddAccount(ctx context.Context, a Account) error {
	env, err := json.Marshal(nonNil(a.Env))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO accounts (name, env, paused, created_at) VALUES (?, ?, ?, ?)`,
		a.Name, string(env), a.Paused, a.CreatedAt.Unix())
	return uniqueErr(err)
}

func (s *Store) SetAccountPaused(ctx context.Context, name string, paused bool) error {
	return affected(s.db.ExecContext(ctx, `UPDATE accounts SET paused = ? WHERE name = ?`, paused, name))
}

func (s *Store) RemoveAccount(ctx context.Context, name string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM accounts WHERE name = ?`, name))
}

// Key is an API key, stored as its SHA-256.
type Key struct {
	ID     int64
	Name   string
	SHA256 [sha256.Size]byte
	Note   string
	// Models limits the key to these client model names; empty allows all.
	Models    []string
	CreatedAt time.Time
	// RevokedAt is zero while the key is valid.
	RevokedAt time.Time
}

func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, sha256, note, models, created_at, revoked_at FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var sum []byte
		var models string
		var created int64
		var revoked sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &sum, &k.Note, &models, &created, &revoked); err != nil {
			return nil, err
		}
		if len(sum) != sha256.Size {
			return nil, errors.New("api key with a malformed hash")
		}
		k.SHA256 = [sha256.Size]byte(sum)
		if err := json.Unmarshal([]byte(models), &k.Models); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(created, 0)
		if revoked.Valid {
			k.RevokedAt = time.Unix(revoked.Int64, 0)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AddKey inserts k and returns its id.
func (s *Store) AddKey(ctx context.Context, k Key) (int64, error) {
	models, err := json.Marshal(nonNil(k.Models))
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_keys (name, sha256, note, models, created_at) VALUES (?, ?, ?, ?, ?)`,
		k.Name, k.SHA256[:], k.Note, string(models), k.CreatedAt.Unix())
	if err != nil {
		return 0, uniqueErr(err)
	}
	return res.LastInsertId()
}

func (s *Store) UpdateKey(ctx context.Context, id int64, note string, models []string) error {
	raw, err := json.Marshal(nonNil(models))
	if err != nil {
		return err
	}
	return affected(s.db.ExecContext(ctx, `UPDATE api_keys SET note = ?, models = ? WHERE id = ?`, note, string(raw), id))
}

func (s *Store) RevokeKey(ctx context.Context, id int64, at time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, at.Unix(), id))
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func uniqueErr(err error) error {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return ErrExists
		}
	}
	return err
}
