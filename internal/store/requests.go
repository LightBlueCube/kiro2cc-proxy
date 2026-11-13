package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Request is one served API request.
type Request struct {
	ID int64
	At time.Time
	// KeyHash identifies the API key; KeyName is its name, for display.
	KeyHash string
	KeyName string
	Account string
	Model   string
	Slug    string
	Effort  string
	API     string
	Stream  bool
	Mode    string
	// ReplayReason says why a replay-mode request reused no session.
	ReplayReason string
	Status       string
	Error        string
	Finish       string
	Duration     time.Duration
	TTFT         *time.Duration
	Input        int64
	Cached       int64
	Output       int64
	Reasoning    int64
	CostUSD      *float64
	QuotaGroup   string
	// QuotaUsed is the attributed share of the weekly quota, 0 to 1.
	QuotaUsed *float64
}

// StatusOK is the Status of a request that succeeded.
const StatusOK = "ok"

const requestColumns = `id, at, key_hash, key_name, account, model, slug, effort, api, stream, mode, replay_reason, status, error, finish,
	duration_ms, ttft_ms, input, cached, output, reasoning, cost_usd, quota_group, quota_used`

// AddRequest stores the request, adds its cost to the key's spend, and
// counts it in the key's totals when it succeeded.
func (s *Store) AddRequest(ctx context.Context, r Request) error {
	var ttft sql.NullInt64
	if r.TTFT != nil {
		ttft = sql.NullInt64{Int64: r.TTFT.Milliseconds(), Valid: true}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO requests (at, key_hash, key_name, account, model, slug, effort, api, stream, mode, replay_reason, status, error, finish,
		duration_ms, ttft_ms, input, cached, output, reasoning, cost_usd, quota_group) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.At.UnixMilli(), r.KeyHash, r.KeyName, r.Account, r.Model, r.Slug, r.Effort, r.API, r.Stream, r.Mode, r.ReplayReason, r.Status, r.Error, r.Finish,
		r.Duration.Milliseconds(), ttft, r.Input, r.Cached, r.Output, r.Reasoning, r.CostUSD, r.QuotaGroup)
	if err != nil {
		return err
	}
	if r.CostUSD != nil && *r.CostUSD > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_usage (key_hash, grp, cost_usd) VALUES (?, ?, ?)
			ON CONFLICT (key_hash, grp) DO UPDATE SET cost_usd = cost_usd + excluded.cost_usd`, r.KeyHash, r.QuotaGroup, *r.CostUSD); err != nil {
			return err
		}
	}
	if r.Status == StatusOK {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_totals (key_hash, model, requests, input, output) VALUES (?, ?, 1, ?, ?)
			ON CONFLICT (key_hash, model) DO UPDATE SET requests = requests + 1, input = input + excluded.input, output = output + excluded.output`,
			r.KeyHash, r.Model, r.Input, r.Output); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RequestFilter selects requests; zero fields do not filter.
type RequestFilter struct {
	KeyHash string
	Model   string
	Account string
	Since   time.Time
	Until   time.Time
}

func (f RequestFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, arg any) {
		conds = append(conds, cond)
		args = append(args, arg)
	}
	if f.KeyHash != "" {
		add("key_hash = ?", f.KeyHash)
	}
	if f.Model != "" {
		add("model = ?", f.Model)
	}
	if f.Account != "" {
		add("account = ?", f.Account)
	}
	if !f.Since.IsZero() {
		add("at >= ?", f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		add("at < ?", f.Until.UnixMilli())
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Requests lists matching requests newest first, at most limit, older than
// beforeID when it is set.
func (s *Store) Requests(ctx context.Context, f RequestFilter, beforeID int64, limit int) ([]Request, error) {
	where, args := f.where()
	if beforeID > 0 {
		if where == "" {
			where = " WHERE id < ?"
		} else {
			where += " AND id < ?"
		}
		args = append(args, beforeID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestColumns+` FROM requests`+where+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		var at, duration int64
		var ttft sql.NullInt64
		var cost, quota sql.NullFloat64
		if err := rows.Scan(&r.ID, &at, &r.KeyHash, &r.KeyName, &r.Account, &r.Model, &r.Slug, &r.Effort, &r.API, &r.Stream, &r.Mode, &r.ReplayReason, &r.Status, &r.Error, &r.Finish,
			&duration, &ttft, &r.Input, &r.Cached, &r.Output, &r.Reasoning, &cost, &r.QuotaGroup, &quota); err != nil {
			return nil, err
		}
		r.At, r.Duration = time.UnixMilli(at), time.Duration(duration)*time.Millisecond
		if ttft.Valid {
			d := time.Duration(ttft.Int64) * time.Millisecond
			r.TTFT = &d
		}
		if cost.Valid {
			r.CostUSD = &cost.Float64
		}
		if quota.Valid {
			r.QuotaUsed = &quota.Float64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Summary totals requests of one group.
type Summary struct {
	Group string `json:"group"`
	// KeyName is the key's name when the group has a key hash.
	KeyName   string  `json:"key_name,omitempty"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	Input     int64   `json:"input"`
	Cached    int64   `json:"cached"`
	Output    int64   `json:"output"`
	Reasoning int64   `json:"reasoning"`
	CostUSD   float64 `json:"cost_usd"`
	// Unpriced counts requests with tokens but no price.
	Unpriced int64 `json:"unpriced"`
}

// SummaryBy names a grouping of Summarize.
type SummaryBy string

const (
	ByKey     SummaryBy = "key"
	ByModel   SummaryBy = "model"
	ByAccount SummaryBy = "account"
	ByDay     SummaryBy = "day"
	// ByDayKey groups by "day\x1fkey hash".
	ByDayKey SummaryBy = "day_key"
)

// Summarize totals matching requests by key hash, model, account, or day.
// Days start at local midnight for a zone tzOffset east of UTC.
func (s *Store) Summarize(ctx context.Context, f RequestFilter, by SummaryBy, tzOffset time.Duration) ([]Summary, error) {
	// A key's name never changes, so any of its requests carries it.
	group, keyName := "", "MIN(key_name)"
	switch by {
	case ByKey:
		group = "key_hash"
	case ByModel:
		group, keyName = "model", "''"
	case ByAccount:
		group, keyName = "account", "''"
	case ByDay:
		group, keyName = fmt.Sprintf("date(at / 1000 + %d, 'unixepoch')", int64(tzOffset.Seconds())), "''"
	case ByDayKey:
		group = fmt.Sprintf("date(at / 1000 + %d, 'unixepoch') || char(31) || key_hash", int64(tzOffset.Seconds()))
	default:
		return nil, fmt.Errorf("unknown grouping %q", by)
	}
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, `SELECT `+group+`, `+keyName+`, COUNT(*), SUM(status != 'ok'), SUM(input), SUM(cached), SUM(output), SUM(reasoning),
		COALESCE(SUM(cost_usd), 0), SUM(cost_usd IS NULL AND input + output > 0)
		FROM requests`+where+` GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var sum Summary
		if err := rows.Scan(&sum.Group, &sum.KeyName, &sum.Requests, &sum.Errors, &sum.Input, &sum.Cached, &sum.Output, &sum.Reasoning, &sum.CostUSD, &sum.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// RequestKey is an API key seen in requests.
type RequestKey struct {
	Hash string `json:"hash"`
	Name string `json:"name"`
}

// RequestKeys lists the keys of requests since the given time by name,
// for filter menus.
func (s *Store) RequestKeys(ctx context.Context, since time.Time) ([]RequestKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_hash, MIN(key_name) FROM requests WHERE at >= ? GROUP BY 1 ORDER BY 2, 1`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestKey
	for rows.Next() {
		var k RequestKey
		if err := rows.Scan(&k.Hash, &k.Name); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Distinct lists the values of a request column (model, account) seen
// since the given time, for filter menus.
func (s *Store) Distinct(ctx context.Context, by SummaryBy, since time.Time) ([]string, error) {
	column := map[SummaryBy]string{ByModel: "model", ByAccount: "account"}[by]
	if column == "" {
		return nil, fmt.Errorf("unknown column %q", by)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT `+column+` FROM requests WHERE at >= ? AND `+column+` != '' ORDER BY 1`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// QuotaWeight is a request's share key in quota attribution.
type QuotaWeight struct {
	ID int64
	// Ended is when the request finished.
	Ended   time.Time
	CostUSD *float64
	Tokens  int64
}

// QuotaWeights lists the account's requests in the quota group with
// tokens, at in (from, to].
func (s *Store) QuotaWeights(ctx context.Context, account, group string, from, to time.Time) ([]QuotaWeight, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at + duration_ms, cost_usd, input + output FROM requests
		WHERE account = ? AND quota_group = ? AND at > ? AND at <= ? AND input + output > 0`, account, group, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaWeight
	for rows.Next() {
		var w QuotaWeight
		var ended int64
		var cost sql.NullFloat64
		if err := rows.Scan(&w.ID, &ended, &cost, &w.Tokens); err != nil {
			return nil, err
		}
		w.Ended = time.UnixMilli(ended)
		if cost.Valid {
			w.CostUSD = &cost.Float64
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// AttributeQuota stores attributed quota shares by request id and moves
// the bucket's attribution cursor to the sample at `through`.
func (s *Store) AttributeQuota(ctx context.Context, account, bucket string, through time.Time, shares map[int64]float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, share := range shares {
		if err := affected(tx.ExecContext(ctx, `UPDATE requests SET quota_used = ? WHERE id = ?`, share, id)); err != nil {
			return fmt.Errorf("request %d: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO quota_cursors (account, bucket, at) VALUES (?, ?, ?)
		ON CONFLICT (account, bucket) DO UPDATE SET at = excluded.at`, account, bucket, through.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// KeySpend is a key's list-price spend in one quota group since the key
// was created; pruning requests keeps it.
type KeySpend struct {
	KeyHash string
	Group   string
	CostUSD float64
}

// KeySpends lists spend by key hash and group; keyHash "" selects every
// key.
func (s *Store) KeySpends(ctx context.Context, keyHash string) ([]KeySpend, error) {
	query, args := `SELECT key_hash, grp, cost_usd FROM key_usage`, []any{}
	if keyHash != "" {
		query += ` WHERE key_hash = ?`
		args = append(args, keyHash)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeySpend
	for rows.Next() {
		var k KeySpend
		if err := rows.Scan(&k.KeyHash, &k.Group, &k.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeyTotal counts a key's successful requests to one model and their
// tokens since the key was created; pruning requests keeps it.
type KeyTotal struct {
	KeyHash  string
	Model    string
	Requests int64
	Input    int64
	Output   int64
}

// KeyTotals lists every key's totals by key hash, then by most tokens.
func (s *Store) KeyTotals(ctx context.Context) ([]KeyTotal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_hash, model, requests, input, output FROM key_totals ORDER BY key_hash, input + output DESC, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyTotal
	for rows.Next() {
		var k KeyTotal
		if err := rows.Scan(&k.KeyHash, &k.Model, &k.Requests, &k.Input, &k.Output); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// QuotaUse is the weekly-quota share attributed to one key's requests on
// one account in one quota group.
type QuotaUse struct {
	KeyHash string  `json:"key_hash"`
	KeyName string  `json:"key_name"`
	Account string  `json:"account"`
	Group   string  `json:"group"`
	Used    float64 `json:"used"`
}

// QuotaUses totals the attributed shares of matching requests by key
// hash, account, and quota group.
func (s *Store) QuotaUses(ctx context.Context, f RequestFilter) ([]QuotaUse, error) {
	where, args := f.where()
	if where == "" {
		where = " WHERE quota_used IS NOT NULL"
	} else {
		where += " AND quota_used IS NOT NULL"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key_hash, MIN(key_name), account, quota_group, SUM(quota_used) FROM requests`+where+`
		GROUP BY 1, 3, 4 ORDER BY 3, 4, 2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaUse
	for rows.Next() {
		var u QuotaUse
		if err := rows.Scan(&u.KeyHash, &u.KeyName, &u.Account, &u.Group, &u.Used); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Prune deletes requests, quota samples, and audit entries older than
// before.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	cutoff := before.UnixMilli()
	for _, table := range []string{"requests", "quota_samples", "audit"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE at < ?`, cutoff); err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
	}
	return nil
}
