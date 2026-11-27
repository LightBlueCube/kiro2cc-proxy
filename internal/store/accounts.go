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
// HOME directory (<accounts>/<ID>), not here.
type Account struct {
	ID   string
	Name string
	// Env is extra environment for the account's agy.
	Env []string
	// Proxy is the HTTP proxy URL agy's traffic goes through ("" = none).
	Proxy string
	// Paused takes the account out of rotation; PauseReason records why
	// agyrelay paused it automatically ("" when paused manually).
	Paused      bool
	PauseReason string
	// Priority orders accounts in rotation; higher values take requests
	// first.
	Priority int
	// Models limits the account to these client model names; empty allows
	// all models the account offers.
	Models    []string
	CreatedAt time.Time
}

func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, env, proxy, paused, pause_reason, priority, models, created_at FROM accounts ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var env, models string
		var created int64
		if err := rows.Scan(&a.ID, &a.Name, &env, &a.Proxy, &a.Paused, &a.PauseReason, &a.Priority, &models, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(env), &a.Env); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(models), &a.Models); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddAccount(ctx context.Context, a Account) error {
	if a.ID == "" {
		a.ID = NewUUID()
	}
	env, err := json.Marshal(nonNil(a.Env))
	if err != nil {
		return err
	}
	models, err := json.Marshal(nonNil(a.Models))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO accounts (id, name, env, proxy, paused, pause_reason, priority, models, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, string(env), a.Proxy, a.Paused, a.PauseReason, a.Priority, string(models), a.CreatedAt.Unix())
	return uniqueErr(err)
}

func (s *Store) RenameAccount(ctx context.Context, id, newName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE accounts SET name = ? WHERE id = ?`, newName, id)
	if err != nil {
		return uniqueErr(err)
	}
	if err := affected(res, nil); err != nil {
		return err
	}
	for _, table := range []string{"requests", "key_quota", "flagged_prompts", "upstream_calls"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET account_name = ? WHERE account = ?`, newName, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetAccountProxy(ctx context.Context, id, proxy string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE accounts SET proxy = ? WHERE id = ?`, proxy, id))
}

func (s *Store) SetAccountRouting(ctx context.Context, id string, priority int, models []string) error {
	encoded, err := json.Marshal(nonNil(models))
	if err != nil {
		return err
	}
	return affected(s.db.ExecContext(ctx, `UPDATE accounts SET priority = ?, models = ? WHERE id = ?`, priority, string(encoded), id))
}

func (s *Store) SetAccountPaused(ctx context.Context, id string, paused bool, reason string) error {
	if !paused {
		reason = ""
	}
	return affected(s.db.ExecContext(ctx, `UPDATE accounts SET paused = ?, pause_reason = ? WHERE id = ?`, paused, reason, id))
}

func (s *Store) RemoveAccount(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id))
}

// Key is an API key, stored as its hash.
type Key struct {
	// Hash is the lowercase hex SHA-256 of the key; it identifies the key.
	Hash string
	Name string
	Note string
	// Models limits the key to these client model names; empty allows all.
	Models []string
	// MaxWorkers caps the key's concurrent workers (open sessions; excess
	// idle ones are closed first); nil is unlimited.
	MaxWorkers *int
	// EvictWaiting lets this key's requests evict its own oldest worker
	// waiting for tool results when worker caps are full.
	EvictWaiting bool
	// AutoRetry retries retryable errors internally instead of returning
	// them to downstream callers.
	AutoRetry bool
	// LimitTotal caps the key's whole list-price spend (key_usage) in USD;
	// nil is no total limit.
	LimitTotal *float64
	// Limits caps the spend per quota group in USD; absent groups are
	// unlimited. At most one of LimitTotal and Limits is set.
	Limits    map[string]float64
	CreatedAt time.Time
	// RevokedAt is zero while the key is valid.
	RevokedAt time.Time
}

const keyColumns = `hash, name, note, models, max_workers, evict_waiting, auto_retry, limit_total, limits, created_at, revoked_at`

func scanKey(scan func(dest ...any) error) (Key, error) {
	var k Key
	var models, limits string
	var maxWorkers sql.NullInt64
	var total sql.NullFloat64
	var created int64
	var revoked sql.NullInt64
	if err := scan(&k.Hash, &k.Name, &k.Note, &models, &maxWorkers, &k.EvictWaiting, &k.AutoRetry, &total, &limits, &created, &revoked); err != nil {
		return Key{}, err
	}
	if maxWorkers.Valid {
		n := int(maxWorkers.Int64)
		k.MaxWorkers = &n
	}
	if total.Valid {
		k.LimitTotal = &total.Float64
	}
	if err := json.Unmarshal([]byte(models), &k.Models); err != nil {
		return Key{}, err
	}
	if err := json.Unmarshal([]byte(limits), &k.Limits); err != nil {
		return Key{}, err
	}
	k.CreatedAt = time.Unix(created, 0)
	if revoked.Valid {
		k.RevokedAt = time.Unix(revoked.Int64, 0)
	}
	return k, nil
}

func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM api_keys ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows.Scan)
		if err != nil {
			return nil, err
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO api_keys (hash, name, note, models, max_workers, evict_waiting, auto_retry, limit_total, limits, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.Hash, k.Name, k.Note, string(models), k.MaxWorkers, k.EvictWaiting, k.AutoRetry, k.LimitTotal, string(limits), k.CreatedAt.Unix())
	return uniqueErr(err)
}

// KeyUpdate is what the admin sets when creating or editing a key.
type KeyUpdate struct {
	Name         string
	Note         string
	Models       []string
	MaxWorkers   *int
	EvictWaiting bool
	AutoRetry    bool
	LimitTotal   *float64
	Limits       map[string]float64
}

// UpdateKey updates a key's editable fields and, when its name changes,
// propagates the new display name to historical requests and flagged prompts.
// It returns the key's previous name.
func (s *Store) UpdateKey(ctx context.Context, hash string, u KeyUpdate) (oldName string, err error) {
	models, err := json.Marshal(nonNil(u.Models))
	if err != nil {
		return "", err
	}
	limits, err := json.Marshal(nonNilMap(u.Limits))
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `SELECT name FROM api_keys WHERE hash = ?`, hash).Scan(&oldName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET name = ?, note = ?, models = ?, max_workers = ?, evict_waiting = ?, auto_retry = ?, limit_total = ?, limits = ? WHERE hash = ?`,
		u.Name, u.Note, string(models), u.MaxWorkers, u.EvictWaiting, u.AutoRetry, u.LimitTotal, string(limits), hash); err != nil {
		return "", uniqueErr(err)
	}
	if u.Name != oldName {
		for _, table := range []string{"requests", "flagged_prompts"} {
			if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET key_name = ? WHERE key_hash = ?`, u.Name, hash); err != nil {
				return "", err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return oldName, nil
}

// KeyUsageReset selects which cumulative counters ResetKeyUsage clears for a
// key. An empty Group clears every quota group; a non-empty Group clears only
// that group. When IncludeStats is true, key_quota and key_totals are cleared
// too; for a single group, MatchModel selects the key_totals models belonging
// to that group.
type KeyUsageReset struct {
	Group        string
	IncludeStats bool
	MatchModel   func(model string) bool
}

// ResetKeyUsage clears a key's cumulative spend in key_usage and, when
// requested, its key_quota and key_totals counters without deleting request
// history. Unsettled requests in the cleared quota scope are marked so later
// attribution still populates requests.quota_used without re-adding them to
// key_quota.
func (s *Store) ResetKeyUsage(ctx context.Context, hash string, r KeyUsageReset) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE hash = ?`, hash).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if r.Group == "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM key_usage WHERE key_hash = ?`, hash); err != nil {
			return err
		}
		if r.IncludeStats {
			if err := clearAllKeyStats(ctx, tx, hash); err != nil {
				return err
			}
		}
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM key_usage WHERE key_hash = ? AND grp = ?`, hash, r.Group); err != nil {
		return err
	}
	if r.IncludeStats {
		if err := clearGroupKeyStats(ctx, tx, hash, r.Group, r.MatchModel); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func clearAllKeyStats(ctx context.Context, tx *sql.Tx, hash string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_quota WHERE key_hash = ?`, hash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET key_quota_reset = 1 WHERE key_hash = ? AND quota_used IS NULL AND quota_skip = ''`, hash); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM key_totals WHERE key_hash = ?`, hash)
	return err
}

func clearGroupKeyStats(ctx context.Context, tx *sql.Tx, hash, group string, matchModel func(string) bool) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_quota WHERE key_hash = ? AND grp = ?`, hash, group); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET key_quota_reset = 1 WHERE key_hash = ? AND quota_group = ? AND quota_used IS NULL AND quota_skip = ''`, hash, group); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT model FROM key_totals WHERE key_hash = ?`, hash)
	if err != nil {
		return err
	}
	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			rows.Close()
			return err
		}
		if matchModel(model) {
			models = append(models, model)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, model := range models {
		if _, err := tx.ExecContext(ctx, `DELETE FROM key_totals WHERE key_hash = ? AND model = ?`, hash, model); err != nil {
			return err
		}
	}
	return nil
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

// DeleteKey deletes a revoked key, its spend, its totals, and its quota use,
// freeing its name, and returns the name. Its requests stay, labeled with the name.
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
	for _, table := range []string{"key_usage", "key_totals", "key_quota"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE key_hash = ?`, hash); err != nil {
			return "", err
		}
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
