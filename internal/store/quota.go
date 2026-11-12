package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// QuotaSample is one bucket's remaining share at one time.
type QuotaSample struct {
	Account   string
	At        time.Time
	Bucket    string
	Group     string
	Window    string
	Remaining float64
	ResetAt   time.Time
}

func (s *Store) AddQuotaSamples(ctx context.Context, samples []QuotaSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range samples {
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO quota_samples (account, at, bucket, grp, win, remaining, reset_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			q.Account, q.At.UnixMilli(), q.Bucket, q.Group, q.Window, q.Remaining, q.ResetAt.Unix())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LastQuotaSample returns the account's newest sample of bucket before at.
func (s *Store) LastQuotaSample(ctx context.Context, account, bucket string, before time.Time) (QuotaSample, error) {
	return scanQuotaSample(s.db.QueryRowContext(ctx, `SELECT account, at, bucket, grp, win, remaining, reset_at FROM quota_samples
		WHERE account = ? AND bucket = ? AND at < ? ORDER BY at DESC LIMIT 1`, account, bucket, before.UnixMilli()))
}

// BucketAccounts lists by bucket the configured accounts that have
// samples of it.
func (s *Store) BucketAccounts(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT bucket, account FROM quota_samples
		WHERE account IN (SELECT name FROM accounts) ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var bucket, account string
		if err := rows.Scan(&bucket, &account); err != nil {
			return nil, err
		}
		out[bucket] = append(out[bucket], account)
	}
	return out, rows.Err()
}

// QuotaCursor returns the sample the bucket's attribution has reached:
// drops up to it are attributed (or known to be nobody's).
func (s *Store) QuotaCursor(ctx context.Context, account, bucket string) (QuotaSample, error) {
	return scanQuotaSample(s.db.QueryRowContext(ctx, `SELECT q.account, q.at, q.bucket, q.grp, q.win, q.remaining, q.reset_at
		FROM quota_cursors c JOIN quota_samples q ON q.account = c.account AND q.bucket = c.bucket AND q.at = c.at
		WHERE c.account = ? AND c.bucket = ?`, account, bucket))
}

func scanQuotaSample(row *sql.Row) (QuotaSample, error) {
	var q QuotaSample
	var at, reset int64
	err := row.Scan(&q.Account, &at, &q.Bucket, &q.Group, &q.Window, &q.Remaining, &reset)
	if errors.Is(err, sql.ErrNoRows) {
		return QuotaSample{}, ErrNotFound
	}
	if err != nil {
		return QuotaSample{}, err
	}
	q.At, q.ResetAt = time.UnixMilli(at), time.Unix(reset, 0)
	return q, nil
}
