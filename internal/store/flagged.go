package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// FlaggedPrompt is a system prompt that Google's risk control refused.
type FlaggedPrompt struct {
	// Fingerprint identifies the prompt (sysprompt.Fingerprint).
	Fingerprint string
	// Text is the prompt as the client sent it; FlaggedPrompts leaves it
	// out, FlaggedPromptText returns it.
	Text string
	// FirstAt and LastAt are when it was first and last refused; Until is
	// when its flag ends.
	FirstAt, LastAt, Until time.Time
	// Hits counts the times it was refused.
	Hits int
	// KeyHash, KeyName, Account, and Model are of its latest refusal.
	KeyHash, KeyName string
	Account, Model   string
}

// FlagPrompt flags a prompt until p.Until. A prompt flagged before keeps
// its text and first time, and gets another hit.
func (s *Store) FlagPrompt(ctx context.Context, p FlaggedPrompt) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO flagged_prompts
		(fingerprint, text, first_at, last_at, flagged_until, hits, key_hash, key_name, account, model)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET
			last_at = excluded.last_at, flagged_until = excluded.flagged_until, hits = hits + 1,
			key_hash = excluded.key_hash, key_name = excluded.key_name, account = excluded.account, model = excluded.model`,
		p.Fingerprint, p.Text, p.LastAt.UnixMilli(), p.LastAt.UnixMilli(), p.Until.UnixMilli(),
		p.KeyHash, p.KeyName, p.Account, p.Model)
	return err
}

// FlaggedPrompts lists every flagged prompt without its text, the latest
// refused first.
func (s *Store) FlaggedPrompts(ctx context.Context) ([]FlaggedPrompt, error) {
	return s.queryFlagged(ctx, `SELECT fingerprint, first_at, last_at, flagged_until, hits, key_hash, key_name, account, model
		FROM flagged_prompts ORDER BY last_at DESC, fingerprint`)
}

// ActiveFlaggedPrompts lists the prompts whose flag has not ended at now,
// without their text.
func (s *Store) ActiveFlaggedPrompts(ctx context.Context, now time.Time) ([]FlaggedPrompt, error) {
	return s.queryFlagged(ctx, `SELECT fingerprint, first_at, last_at, flagged_until, hits, key_hash, key_name, account, model
		FROM flagged_prompts WHERE flagged_until > ? ORDER BY last_at DESC, fingerprint`, now.UnixMilli())
}

func (s *Store) queryFlagged(ctx context.Context, query string, args ...any) ([]FlaggedPrompt, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlaggedPrompt
	for rows.Next() {
		var p FlaggedPrompt
		var first, last, until int64
		if err := rows.Scan(&p.Fingerprint, &first, &last, &until, &p.Hits, &p.KeyHash, &p.KeyName, &p.Account, &p.Model); err != nil {
			return nil, err
		}
		p.FirstAt, p.LastAt, p.Until = time.UnixMilli(first), time.UnixMilli(last), time.UnixMilli(until)
		out = append(out, p)
	}
	return out, rows.Err()
}

// FlaggedPromptText returns the text of a flagged prompt.
func (s *Store) FlaggedPromptText(ctx context.Context, fingerprint string) (string, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT text FROM flagged_prompts WHERE fingerprint = ?`, fingerprint).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return text, err
}

// CancelFlaggedPrompt ends the flag of a prompt at now; the row stays. It
// returns ErrNotFound when the prompt has no flag running at now.
func (s *Store) CancelFlaggedPrompt(ctx context.Context, fingerprint string, now time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE flagged_prompts SET flagged_until = ? WHERE fingerprint = ? AND flagged_until > ?`,
		now.UnixMilli(), fingerprint, now.UnixMilli()))
}
