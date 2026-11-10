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
	ID         int64
	At         time.Time
	Key        string
	Account    string
	Model      string
	Slug       string
	API        string
	Stream     bool
	Mode       string
	Status     string
	Error      string
	Finish     string
	Duration   time.Duration
	TTFT       *time.Duration
	Input      int64
	Cached     int64
	Output     int64
	Reasoning  int64
	CostUSD    *float64
	QuotaGroup string
	// QuotaUsed is the attributed share of the weekly quota, 0 to 1.
	QuotaUsed *float64
}

const requestColumns = `id, at, key_name, account, model, slug, api, stream, mode, status, error, finish,
	duration_ms, ttft_ms, input, cached, output, reasoning, cost_usd, quota_group, quota_used`

func (s *Store) AddRequest(ctx context.Context, r Request) error {
	var ttft sql.NullInt64
	if r.TTFT != nil {
		ttft = sql.NullInt64{Int64: r.TTFT.Milliseconds(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO requests (at, key_name, account, model, slug, api, stream, mode, status, error, finish,
		duration_ms, ttft_ms, input, cached, output, reasoning, cost_usd, quota_group) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.At.UnixMilli(), r.Key, r.Account, r.Model, r.Slug, r.API, r.Stream, r.Mode, r.Status, r.Error, r.Finish,
		r.Duration.Milliseconds(), ttft, r.Input, r.Cached, r.Output, r.Reasoning, r.CostUSD, r.QuotaGroup)
	return err
}

// RequestFilter selects requests; zero fields do not filter.
type RequestFilter struct {
	Key     string
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
	if f.Key != "" {
		add("key_name = ?", f.Key)
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
		if err := rows.Scan(&r.ID, &at, &r.Key, &r.Account, &r.Model, &r.Slug, &r.API, &r.Stream, &r.Mode, &r.Status, &r.Error, &r.Finish,
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
	Group     string  `json:"group"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	Input     int64   `json:"input"`
	Cached    int64   `json:"cached"`
	Output    int64   `json:"output"`
	Reasoning int64   `json:"reasoning"`
	CostUSD   float64 `json:"cost_usd"`
	// Unpriced counts requests with tokens but no price.
	Unpriced  int64   `json:"unpriced"`
	QuotaUsed float64 `json:"quota_used"`
}

// SummaryBy names a grouping of Summarize.
type SummaryBy string

const (
	ByKey     SummaryBy = "key"
	ByModel   SummaryBy = "model"
	ByAccount SummaryBy = "account"
	ByDay     SummaryBy = "day"
	// ByDayKey groups by "day\x1fkey".
	ByDayKey SummaryBy = "day_key"
)

// Summarize totals matching requests by key, model, account, or day. Days
// start at local midnight for a zone tzOffset east of UTC.
func (s *Store) Summarize(ctx context.Context, f RequestFilter, by SummaryBy, tzOffset time.Duration) ([]Summary, error) {
	var group string
	switch by {
	case ByKey:
		group = "key_name"
	case ByModel:
		group = "model"
	case ByAccount:
		group = "account"
	case ByDay:
		group = fmt.Sprintf("date(at / 1000 + %d, 'unixepoch')", int64(tzOffset.Seconds()))
	case ByDayKey:
		group = fmt.Sprintf("date(at / 1000 + %d, 'unixepoch') || char(31) || key_name", int64(tzOffset.Seconds()))
	default:
		return nil, fmt.Errorf("unknown grouping %q", by)
	}
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, `SELECT `+group+`, COUNT(*), SUM(status != 'ok'), SUM(input), SUM(cached), SUM(output), SUM(reasoning),
		COALESCE(SUM(cost_usd), 0), SUM(cost_usd IS NULL AND input + output > 0), COALESCE(SUM(quota_used), 0)
		FROM requests`+where+` GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var sum Summary
		if err := rows.Scan(&sum.Group, &sum.Requests, &sum.Errors, &sum.Input, &sum.Cached, &sum.Output, &sum.Reasoning, &sum.CostUSD, &sum.Unpriced, &sum.QuotaUsed); err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// Distinct lists the values of a request column (key_name, model,
// account) seen since the given time, for filter menus.
func (s *Store) Distinct(ctx context.Context, by SummaryBy, since time.Time) ([]string, error) {
	column := map[SummaryBy]string{ByKey: "key_name", ByModel: "model", ByAccount: "account"}[by]
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
	ID      int64
	CostUSD *float64
	Tokens  int64
}

// QuotaWeights lists the account's requests in the quota group with
// tokens, at in (from, to].
func (s *Store) QuotaWeights(ctx context.Context, account, group string, from, to time.Time) ([]QuotaWeight, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, cost_usd, input + output FROM requests
		WHERE account = ? AND quota_group = ? AND at > ? AND at <= ? AND input + output > 0`, account, group, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaWeight
	for rows.Next() {
		var w QuotaWeight
		var cost sql.NullFloat64
		if err := rows.Scan(&w.ID, &cost, &w.Tokens); err != nil {
			return nil, err
		}
		if cost.Valid {
			w.CostUSD = &cost.Float64
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetQuotaUsed stores attributed quota shares by request id.
func (s *Store) SetQuotaUsed(ctx context.Context, shares map[int64]float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, share := range shares {
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET quota_used = ? WHERE id = ?`, share, id); err != nil {
			return err
		}
	}
	return tx.Commit()
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
