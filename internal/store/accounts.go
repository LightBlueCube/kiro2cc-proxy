package store

import (
	"context"
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
	// Env is extra environment for the account's agy.
	Env []string
	// Proxy is the HTTP proxy URL agy's traffic goes through ("" = none).
	Proxy string
	// Paused takes the account out of rotation.
	Paused    bool
	CreatedAt time.Time
}

func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, env, proxy, paused, created_at FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var env string
		var created int64
		if err := rows.Scan(&a.Name, &env, &a.Proxy, &a.Paused, &created); err != nil {
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO accounts (name, env, proxy, paused, created_at) VALUES (?, ?, ?, ?, ?)`,
		a.Name, string(env), a.Proxy, a.Paused, a.CreatedAt.Unix())
	return uniqueErr(err)
}

func (s *Store) SetAccountProxy(ctx context.Context, name, proxy string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE accounts SET proxy = ? WHERE name = ?`, proxy, name))
}

func (s *Store) SetAccountPaused(ctx context.Context, name string, paused bool) error {
	return affected(s.db.ExecContext(ctx, `UPDATE accounts SET paused = ? WHERE name = ?`, paused, name))
}

func (s *Store) RemoveAccount(ctx context.Context, name string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM accounts WHERE name = ?`, name))
}

// Key is an API key, stored as its hash.
type Key struct {
	// Hash is the lowercase hex SHA-256 of the key; it identifies the key.
	Hash string
	Name string
	Note string
	// Models limits the key to these client model names; empty allows all.
	Models []string
	// Limits caps the key's total quota use (key_usage) per quota group,
	// as a share of one account's weekly bucket (0.3 = 30%); absent
	// groups are unlimited.
	Limits    map[string]float64
	CreatedAt time.Time
	// RevokedAt is zero while the key is valid.
	RevokedAt time.Time
}

func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash, name, note, models, limits, created_at, revoked_at FROM api_keys ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var models, limits string
		var created int64
		var revoked sql.NullInt64
		if err := rows.Scan(&k.Hash, &k.Name, &k.Note, &models, &limits, &created, &revoked); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(models), &k.Models); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(limits), &k.Limits); err != nil {
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

func (s *Store) AddKey(ctx context.Context, k Key) error {
	models, err := json.Marshal(nonNil(k.Models))
	if err != nil {
		return err
	}
	limits, err := json.Marshal(nonNilMap(k.Limits))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO api_keys (hash, name, note, models, limits, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		k.Hash, k.Name, k.Note, string(models), string(limits), k.CreatedAt.Unix())
	return uniqueErr(err)
}

// KeyUpdate is what the admin can change on a key.
type KeyUpdate struct {
	Note   string
	Models []string
	Limits map[string]float64
}

func (s *Store) UpdateKey(ctx context.Context, hash string, u KeyUpdate) error {
	models, err := json.Marshal(nonNil(u.Models))
	if err != nil {
		return err
	}
	limits, err := json.Marshal(nonNilMap(u.Limits))
	if err != nil {
		return err
	}
	return affected(s.db.ExecContext(ctx, `UPDATE api_keys SET note = ?, models = ?, limits = ? WHERE hash = ?`,
		u.Note, string(models), string(limits), hash))
}

func (s *Store) RevokeKey(ctx context.Context, hash string, at time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE hash = ? AND revoked_at IS NULL`, at.Unix(), hash))
}

// RestoreKey makes a revoked key valid again.
func (s *Store) RestoreKey(ctx context.Context, hash string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = NULL WHERE hash = ? AND revoked_at IS NOT NULL`, hash))
}

// ErrKeyActive means the key must be revoked first.
var ErrKeyActive = errors.New("key is not revoked")

// DeleteKey deletes a revoked key and its quota use, freeing its name, and
// returns the name. Its requests stay, labeled with the name.
func (s *Store) DeleteKey(ctx context.Context, hash string) (name string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var revoked sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT name, revoked_at FROM api_keys WHERE hash = ?`, hash).Scan(&name, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !revoked.Valid {
		return "", ErrKeyActive
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE hash = ?`, hash); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_usage WHERE key_hash = ?`, hash); err != nil {
		return "", err
	}
	return name, tx.Commit()
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func nonNilMap(m map[string]float64) map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	return m
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
