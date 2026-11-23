package store

import (
	"context"
	"database/sql"
	"errors"
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
	// Account identifies the account (its ID); AccountName is its name,
	// for display.
	Account     string
	AccountName string
	Model       string
	Slug        string
	Effort      string
	API         string
	Stream      bool
	Anchor      string
	Mode        string
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
	// QuotaGroup is the quota group the serving account charged.
	QuotaGroup string
	// KeyGroup is the group of the key's spend limit the cost counts in;
	// it is not stored with the request, only in the key's total.
	KeyGroup string
	// QuotaUsed is the attributed share of the weekly quota, 0 to 1.
	QuotaUsed *float64
	// QuotaSkip, when set, is why the request gets no QuotaUsed.
	QuotaSkip QuotaSkip
	// CallToken ties the request to the upstream calls captured for it.
	CallToken string
	// Session is the ACP session that served the request, empty when no
	// attempt started.
	Session string
	// Upstream counts the request's captured upstream calls still kept;
	// Requests fills it, AddRequest ignores it.
	Upstream int
}

// QuotaSkip is why a request's quota use cannot be attributed.
type QuotaSkip string

const (
	// SkipBeforeSampling: the bucket was first sampled after the request.
	SkipBeforeSampling QuotaSkip = "before_sampling"
	// SkipReset: the bucket reset before the request's use was settled.
	SkipReset QuotaSkip = "reset"
	// SkipNoBucket: the account's plan has no weekly bucket for the
	// request's quota group.
	SkipNoBucket QuotaSkip = "no_bucket"
	// SkipFailed: the request waited past pendingMaxAge without showing
	// a measurable quota drop.
	SkipFailed QuotaSkip = "failed"
)

// StatusOK is the Status of a request that succeeded.
const StatusOK = "ok"

// StatusCancelled is the Status of a request whose client disconnected or
// whose turn was cancelled.
const StatusCancelled = "cancelled"

// SystemKeyWarmup is the synthetic key_hash for automatic weekly-quota warmup
// calls. It is not a SHA-256 hex digest, so it never collides with an API key,
// never appears in /user/ queries, and is excluded from key_* billing tables.
const SystemKeyWarmup = "system:warmup"

// IsSystemKey reports whether keyHash belongs to an internal relay call rather
// than a client API key.
func IsSystemKey(keyHash string) bool {
	return keyHash == "" || strings.HasPrefix(keyHash, "system:")
}

const requestAccountNameExpr = `COALESCE((SELECT name FROM accounts WHERE id = requests.account), NULLIF(requests.account_name, ''), requests.account)`

const requestColumns = `id, at, key_hash, key_name, account, ` + requestAccountNameExpr + `, model, slug, effort, api, stream, anchor, mode, replay_reason, status, error, finish,
	duration_ms, ttft_ms, input, cached, output, reasoning, cost_usd, quota_group, quota_used, quota_skip, call_token, session`

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
	_, err = tx.ExecContext(ctx, `INSERT INTO requests (at, key_hash, key_name, account, account_name, model, slug, effort, api, stream, anchor, mode, replay_reason, status, error, finish,
		duration_ms, ttft_ms, input, cached, output, reasoning, cost_usd, quota_group, call_token, session) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.At.UnixMilli(), r.KeyHash, r.KeyName, r.Account, r.AccountName, r.Model, r.Slug, r.Effort, r.API, r.Stream, r.Anchor, r.Mode, r.ReplayReason, r.Status, r.Error, r.Finish,
		r.Duration.Milliseconds(), ttft, r.Input, r.Cached, r.Output, r.Reasoning, r.CostUSD, r.QuotaGroup, r.CallToken, r.Session)
	if err != nil {
		return err
	}
	if !IsSystemKey(r.KeyHash) && r.CostUSD != nil && *r.CostUSD > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_usage (key_hash, grp, cost_usd) VALUES (?, ?, ?)
			ON CONFLICT (key_hash, grp) DO UPDATE SET cost_usd = cost_usd + excluded.cost_usd`, r.KeyHash, r.KeyGroup, *r.CostUSD); err != nil {
			return err
		}
	}
	if !IsSystemKey(r.KeyHash) && r.Status == StatusOK {
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
	upstream := `(SELECT COUNT(*) FROM upstream_calls u WHERE u.call_token = requests.call_token AND requests.call_token != '' AND u.at >= ?)`
	args = append([]any{upstreamCutoff()}, args...)
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestColumns+`, `+upstream+` FROM requests`+where+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
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
		if err := rows.Scan(&r.ID, &at, &r.KeyHash, &r.KeyName, &r.Account, &r.AccountName, &r.Model, &r.Slug, &r.Effort, &r.API, &r.Stream, &r.Anchor, &r.Mode, &r.ReplayReason, &r.Status, &r.Error, &r.Finish,
			&duration, &ttft, &r.Input, &r.Cached, &r.Output, &r.Reasoning, &cost, &r.QuotaGroup, &quota, &r.QuotaSkip, &r.CallToken, &r.Session, &r.Upstream); err != nil {
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
	KeyName string `json:"key_name,omitempty"`
	// AccountName is the account's name when grouped by account.
	AccountName string  `json:"account_name,omitempty"`
	Requests    int64   `json:"requests"`
	Errors      int64   `json:"errors"`
	Input       int64   `json:"input"`
	Cached      int64   `json:"cached"`
	Output      int64   `json:"output"`
	Reasoning   int64   `json:"reasoning"`
	CostUSD     float64 `json:"cost_usd"`
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
	group, keyName, accountName := "", "MIN(key_name)", "''"
	switch by {
	case ByKey:
		group = "key_hash"
	case ByModel:
		group, keyName = "model", "''"
	case ByAccount:
		group, keyName = "account", "''"
		accountName = `COALESCE((SELECT name FROM accounts WHERE id = requests.account), NULLIF(MAX(account_name), ''), account)`
	case ByDay:
		group, keyName = fmt.Sprintf("date(at / 1000 + %d, 'unixepoch')", int64(tzOffset.Seconds())), "''"
	case ByDayKey:
		group = fmt.Sprintf("date(at / 1000 + %d, 'unixepoch') || char(31) || key_hash", int64(tzOffset.Seconds()))
	default:
		return nil, fmt.Errorf("unknown grouping %q", by)
	}
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, `SELECT `+group+`, `+keyName+`, `+accountName+`, COUNT(*), SUM(status != 'ok' AND status != 'cancelled'), SUM(input), SUM(cached), SUM(output), SUM(reasoning),
		COALESCE(SUM(cost_usd), 0), SUM(cost_usd IS NULL AND input + output > 0)
		FROM requests`+where+` GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var sum Summary
		if err := rows.Scan(&sum.Group, &sum.KeyName, &sum.AccountName, &sum.Requests, &sum.Errors, &sum.Input, &sum.Cached, &sum.Output, &sum.Reasoning, &sum.CostUSD, &sum.Unpriced); err != nil {
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

// RequestAccount is an account seen in requests.
type RequestAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// RequestAccounts lists the accounts of matching requests by name, for
// filter menus.
func (s *Store) RequestAccounts(ctx context.Context, f RequestFilter) ([]RequestAccount, error) {
	where, args := f.where()
	if where == "" {
		where = " WHERE account != ''"
	} else {
		where += " AND account != ''"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT account, COALESCE((SELECT name FROM accounts WHERE id = requests.account), NULLIF(MAX(account_name), ''), account)
		FROM requests`+where+` GROUP BY 1 ORDER BY 2, 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestAccount
	for rows.Next() {
		var a RequestAccount
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Distinct lists the values of a request column (model, account) in
// matching requests, for filter menus.
func (s *Store) Distinct(ctx context.Context, by SummaryBy, f RequestFilter) ([]string, error) {
	column := map[SummaryBy]string{ByModel: "model", ByAccount: "account"}[by]
	if column == "" {
		return nil, fmt.Errorf("unknown column %q", by)
	}
	where, args := f.where()
	if where == "" {
		where = " WHERE " + column + " != ''"
	} else {
		where += " AND " + column + " != ''"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT `+column+` FROM requests`+where+` ORDER BY 1`, args...)
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

// unsettled selects the account's requests in a quota group with tokens
// that ended by a time and have neither a quota share nor a skip reason.
// By end time, not start: a request stored after a sample passed its
// start is still settled by a later one.
const unsettled = `account = ? AND quota_group = ? AND quota_used IS NULL AND quota_skip = '' AND input + output > 0 AND at + duration_ms <= ?`

// QuotaWeights lists the unsettled requests that ended by through.
func (s *Store) QuotaWeights(ctx context.Context, account, group string, through time.Time) ([]QuotaWeight, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at + duration_ms, cost_usd, input + output FROM requests WHERE `+unsettled,
		account, group, through.UnixMilli())
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

// AttributeQuota stores attributed quota shares by request id, adds them
// to key_quota, and moves the bucket's attribution cursor to the sample
// at `through`.
func (s *Store) AttributeQuota(ctx context.Context, account, bucket string, through time.Time, shares map[int64]float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, share := range shares {
		var keyHash, reqAccount, reqAccountName, group string
		err := tx.QueryRowContext(ctx,
			`UPDATE requests SET quota_used = ? WHERE id = ? AND quota_used IS NULL RETURNING key_hash, account, `+requestAccountNameExpr+`, quota_group`,
			share, id,
		).Scan(&keyHash, &reqAccount, &reqAccountName, &group)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("request %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("request %d: %w", id, err)
		}
		if share > 0 && !IsSystemKey(keyHash) && reqAccount != "" && group != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO key_quota (key_hash, account, account_name, grp, used) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (key_hash, account, grp) DO UPDATE SET used = used + excluded.used,
					account_name = CASE WHEN excluded.account_name != '' THEN excluded.account_name ELSE key_quota.account_name END`,
				keyHash, reqAccount, reqAccountName, group, share); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO quota_cursors (account, bucket, at) VALUES (?, ?, ?)
		ON CONFLICT (account, bucket) DO UPDATE SET at = excluded.at`, account, bucket, through.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// HasUnsettledQuota reports whether the account has requests with tokens
// that have neither a quota share nor a skip reason.
func (s *Store) HasUnsettledQuota(ctx context.Context, account string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM requests
		WHERE account = ? AND quota_used IS NULL AND quota_skip = '' AND input + output > 0)`, account).Scan(&found)
	return found, err
}

// SkipQuota gives the unsettled requests that ended by through a skip
// reason instead of a quota share.
func (s *Store) SkipQuota(ctx context.Context, account, group string, through time.Time, reason QuotaSkip) error {
	_, err := s.db.ExecContext(ctx, `UPDATE requests SET quota_skip = ? WHERE `+unsettled, reason, account, group, through.UnixMilli())
	return err
}

// ExpirePendingQuota marks unsettled requests across all accounts that
// ended by before with reason, returning the number of rows updated.
func (s *Store) ExpirePendingQuota(ctx context.Context, before time.Time, reason QuotaSkip) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE requests SET quota_skip = ? WHERE quota_used IS NULL AND quota_skip = '' AND input + output > 0 AND at + duration_ms <= ?`,
		reason, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

// KeyTotals lists totals by key hash, then by most tokens; keyHash ""
// selects every key.
func (s *Store) KeyTotals(ctx context.Context, keyHash string) ([]KeyTotal, error) {
	query, args := `SELECT key_hash, model, requests, input, output FROM key_totals`, []any{}
	if keyHash != "" {
		query += ` WHERE key_hash = ?`
		args = append(args, keyHash)
	}
	query += ` ORDER BY key_hash, input + output DESC, model`
	rows, err := s.db.QueryContext(ctx, query, args...)
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

// KeyQuota is a key's attributed weekly-quota share on one account in one
// quota group since the key was created; pruning requests keeps it.
type KeyQuota struct {
	KeyHash     string
	Account     string
	AccountName string
	Group       string
	Used        float64
}

// KeyQuotas lists attributed quota by key hash, then by quota group and
// largest account share; keyHash "" selects every key.
func (s *Store) KeyQuotas(ctx context.Context, keyHash string) ([]KeyQuota, error) {
	nameExpr := `COALESCE((SELECT name FROM accounts WHERE id = key_quota.account), NULLIF(key_quota.account_name, ''), key_quota.account)`
	query, args := `SELECT key_hash, account, `+nameExpr+`, grp, used FROM key_quota`, []any{}
	if keyHash != "" {
		query += ` WHERE key_hash = ?`
		args = append(args, keyHash)
	}
	query += ` ORDER BY key_hash, grp, used DESC, 3, account`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyQuota
	for rows.Next() {
		var q KeyQuota
		if err := rows.Scan(&q.KeyHash, &q.Account, &q.AccountName, &q.Group, &q.Used); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// QuotaUse is the weekly-quota share attributed to one key's requests on
// one account in one quota group.
type QuotaUse struct {
	KeyHash     string  `json:"key_hash"`
	KeyName     string  `json:"key_name"`
	Account     string  `json:"account"`
	AccountName string  `json:"account_name"`
	Group       string  `json:"group"`
	Used        float64 `json:"used"`
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
	nameExpr := `COALESCE((SELECT name FROM accounts WHERE id = requests.account), NULLIF(MAX(requests.account_name), ''), requests.account)`
	rows, err := s.db.QueryContext(ctx, `SELECT key_hash, MIN(key_name), account, `+nameExpr+`, quota_group, SUM(quota_used) FROM requests`+where+`
		GROUP BY 1, 3, 5 ORDER BY 4, 3, 5, 2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaUse
	for rows.Next() {
		var u QuotaUse
		if err := rows.Scan(&u.KeyHash, &u.KeyName, &u.Account, &u.AccountName, &u.Group, &u.Used); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ModelUsage totals requests to one model on an account in one quota group.
type ModelUsage struct {
	Model     string  `json:"model"`
	Requests  int64   `json:"requests"`
	Input     int64   `json:"input"`
	Cached    int64   `json:"cached"`
	Output    int64   `json:"output"`
	Reasoning int64   `json:"reasoning"`
	CostUSD   float64 `json:"cost_usd"`
	Unpriced  int64   `json:"unpriced"`
}

// AccountGroupUsage totals the account's requests in group whose end
// time (at + duration_ms) is at or after since, grouped by model and
// ordered by list-price spend then total tokens descending. Only requests
// that used tokens count.
func (s *Store) AccountGroupUsage(ctx context.Context, account, group string, since time.Time) ([]ModelUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model, COUNT(*), SUM(input), SUM(cached), SUM(output), SUM(reasoning),
		COALESCE(SUM(cost_usd), 0), SUM(cost_usd IS NULL)
		FROM requests
		WHERE account = ? AND quota_group = ? AND at + duration_ms >= ? AND input + output > 0
		GROUP BY model
		ORDER BY COALESCE(SUM(cost_usd), 0) DESC, SUM(input + output) DESC, model`,
		account, group, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelUsage
	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Requests, &m.Input, &m.Cached, &m.Output, &m.Reasoning, &m.CostUSD, &m.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LastAccountGroupRequestEnd returns the latest end time (at + duration_ms) of
// a token-using request on account in quota group that ended at or after since,
// or zero when none exists.
func (s *Store) LastAccountGroupRequestEnd(ctx context.Context, account, group string, since time.Time) (time.Time, error) {
	// Bounding at >= since - 1h lets SQLite range-scan requests_account_at while
	// still matching long-running turns that began before since.
	minStart := since.Add(-time.Hour)
	var latest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(at + duration_ms) FROM requests
		WHERE account = ? AND quota_group = ? AND at >= ? AND at + duration_ms >= ? AND input + output > 0`,
		account, group, minStart.UnixMilli(), since.UnixMilli()).Scan(&latest)
	if err != nil {
		return time.Time{}, err
	}
	if !latest.Valid {
		return time.Time{}, nil
	}
	return time.UnixMilli(latest.Int64), nil
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
