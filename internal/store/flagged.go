package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// FlaggedPrompt is a system prompt that Google's risk control refused.
type FlaggedPrompt struct {
	// Fingerprint identifies the prompt (sysprompt.Fingerprint).
	Fingerprint string
	// Text is the prompt as the client sent it; Request and Response are
	// the Cloud Code generation call and error body of its latest refusal.
	// FlaggedPrompts leaves all three out; FlaggedPrompt returns them.
	Text              string
	Request, Response []byte
	// FirstAt and LastAt are when it was first and last refused; Until is
	// when its flag ends.
	FirstAt, LastAt, Until time.Time
	// Pinned keeps the prompt refused past Until, and out of
	// PruneFlaggedPrompts, until the admin unpins or cancels it.
	Pinned bool
	// Hits counts the times it was refused.
	Hits int
	// KeyHash, KeyName, Account, AccountName, Session, and Model are of
	// its latest refusal.
	KeyHash, KeyName     string
	Account, AccountName string
	Session              string
	Model                string
}

// FlagPrompt flags a prompt until p.Until. A prompt flagged before keeps
// its text and first time, and gets another hit with the latest refusal's
// metadata and upstream call.
func (s *Store) FlagPrompt(ctx context.Context, p FlaggedPrompt) error {
	request, err := packOptional(p.Request)
	if err != nil {
		return err
	}
	response, err := packOptional(p.Response)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO flagged_prompts
		(fingerprint, text, first_at, last_at, flagged_until, hits, key_hash, key_name, account, account_name, session, model, request, response)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET
			last_at = excluded.last_at, flagged_until = excluded.flagged_until, hits = hits + 1,
			key_hash = excluded.key_hash, key_name = excluded.key_name, account = excluded.account, account_name = excluded.account_name,
			session = excluded.session, model = excluded.model, request = excluded.request, response = excluded.response`,
		p.Fingerprint, p.Text, p.LastAt.UnixMilli(), p.LastAt.UnixMilli(), p.Until.UnixMilli(),
		p.KeyHash, p.KeyName, p.Account, p.AccountName, p.Session, p.Model, request, response)
	return err
}

const flaggedAccountNameExpr = `COALESCE((SELECT name FROM accounts WHERE id = flagged_prompts.account), NULLIF(account_name, ''), account)`

// FlaggedPrompts lists every flagged prompt without its bodies, the latest
// refused first.
func (s *Store) FlaggedPrompts(ctx context.Context) ([]FlaggedPrompt, error) {
	return s.queryFlagged(ctx, `SELECT fingerprint, first_at, last_at, flagged_until, pinned, hits, key_hash, key_name, account, `+flaggedAccountNameExpr+`, session, model
		FROM flagged_prompts ORDER BY last_at DESC, fingerprint`)
}

// ActiveFlaggedPrompts lists the prompts that are pinned or whose flag has
// not ended at now, without their bodies.
func (s *Store) ActiveFlaggedPrompts(ctx context.Context, now time.Time) ([]FlaggedPrompt, error) {
	return s.queryFlagged(ctx, `SELECT fingerprint, first_at, last_at, flagged_until, pinned, hits, key_hash, key_name, account, `+flaggedAccountNameExpr+`, session, model
		FROM flagged_prompts WHERE pinned = 1 OR flagged_until > ? ORDER BY last_at DESC, fingerprint`, now.UnixMilli())
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
		if err := rows.Scan(&p.Fingerprint, &first, &last, &until, &p.Pinned, &p.Hits, &p.KeyHash, &p.KeyName, &p.Account, &p.AccountName, &p.Session, &p.Model); err != nil {
			return nil, err
		}
		p.FirstAt, p.LastAt, p.Until = time.UnixMilli(first), time.UnixMilli(last), time.UnixMilli(until)
		out = append(out, p)
	}
	return out, rows.Err()
}

// FlaggedPrompt returns one flagged prompt with its text and captured
// upstream call, or ErrNotFound.
func (s *Store) FlaggedPrompt(ctx context.Context, fingerprint string) (FlaggedPrompt, error) {
	var p FlaggedPrompt
	var first, last, until int64
	var request, response []byte
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint, text, first_at, last_at, flagged_until, pinned, hits,
		key_hash, key_name, account, `+flaggedAccountNameExpr+`, session, model, request, response
		FROM flagged_prompts WHERE fingerprint = ?`, fingerprint).
		Scan(&p.Fingerprint, &p.Text, &first, &last, &until, &p.Pinned, &p.Hits,
			&p.KeyHash, &p.KeyName, &p.Account, &p.AccountName, &p.Session, &p.Model, &request, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return FlaggedPrompt{}, ErrNotFound
	}
	if err != nil {
		return FlaggedPrompt{}, err
	}
	p.FirstAt, p.LastAt, p.Until = time.UnixMilli(first), time.UnixMilli(last), time.UnixMilli(until)
	if p.Request, err = unpackOptional(request); err != nil {
		return FlaggedPrompt{}, fmt.Errorf("flagged prompt %s request: %w", fingerprint, err)
	}
	if p.Response, err = unpackOptional(response); err != nil {
		return FlaggedPrompt{}, fmt.Errorf("flagged prompt %s response: %w", fingerprint, err)
	}
	return p, nil
}

func packOptional(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return []byte{}, nil
	}
	return compress(raw)
}

func unpackOptional(packed []byte) ([]byte, error) {
	if len(packed) == 0 {
		return nil, nil
	}
	return decompress(packed)
}

// CancelFlaggedPrompt ends the flag of a prompt at now and unpins it; the
// row stays. It returns ErrNotFound when the prompt is neither pinned nor
// flagged at now.
func (s *Store) CancelFlaggedPrompt(ctx context.Context, fingerprint string, now time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE flagged_prompts
		SET flagged_until = ?, pinned = 0, released_at = CASE WHEN pinned = 1 THEN ? ELSE released_at END
		WHERE fingerprint = ? AND (pinned = 1 OR flagged_until > ?)`,
		now.UnixMilli(), now.UnixMilli(), fingerprint, now.UnixMilli()))
}

// PinFlaggedPrompt keeps a prompt refused until it is unpinned. It returns
// ErrNotFound when there is no such prompt.
func (s *Store) PinFlaggedPrompt(ctx context.Context, fingerprint string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE flagged_prompts SET pinned = 1 WHERE fingerprint = ?`, fingerprint))
}

// UnpinFlaggedPrompt lets a pinned prompt's flag run out, and restarts its
// retention at now. It returns ErrNotFound when the prompt is not pinned.
func (s *Store) UnpinFlaggedPrompt(ctx context.Context, fingerprint string, now time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE flagged_prompts SET pinned = 0, released_at = ? WHERE fingerprint = ? AND pinned = 1`,
		now.UnixMilli(), fingerprint))
}

// DeleteFlaggedPrompt removes a flagged prompt from the store. It returns
// ErrNotFound when there is no such prompt.
func (s *Store) DeleteFlaggedPrompt(ctx context.Context, fingerprint string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM flagged_prompts WHERE fingerprint = ?`, fingerprint))
}

// PruneFlaggedPrompts deletes the unpinned prompts that were neither
// refused nor released since before.
func (s *Store) PruneFlaggedPrompts(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM flagged_prompts WHERE pinned = 0 AND last_at < ? AND released_at < ?`,
		before.UnixMilli(), before.UnixMilli())
	return err
}
