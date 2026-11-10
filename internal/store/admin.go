package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AdminSession is a signed-in admin browser. Only the token's hash is
// stored; PublicID names the session in the UI.
type AdminSession struct {
	TokenHash []byte
	PublicID  string
	CSRF      string
	CreatedAt time.Time
	SeenAt    time.Time
	ExpiresAt time.Time
	IP        string
	UserAgent string
}

func (s *Store) AddAdminSession(ctx context.Context, a AdminSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_sessions (token_hash, public_id, csrf, created_at, seen_at, expires_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TokenHash, a.PublicID, a.CSRF, a.CreatedAt.Unix(), a.SeenAt.Unix(), a.ExpiresAt.Unix(), a.IP, a.UserAgent)
	return err
}

func (s *Store) AdminSession(ctx context.Context, tokenHash []byte) (AdminSession, error) {
	row := s.db.QueryRowContext(ctx, `SELECT token_hash, public_id, csrf, created_at, seen_at, expires_at, ip, user_agent
		FROM admin_sessions WHERE token_hash = ?`, tokenHash)
	a, err := scanAdminSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminSession{}, ErrNotFound
	}
	return a, err
}

func (s *Store) AdminSessions(ctx context.Context) ([]AdminSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token_hash, public_id, csrf, created_at, seen_at, expires_at, ip, user_agent
		FROM admin_sessions ORDER BY seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminSession
	for rows.Next() {
		a, err := scanAdminSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAdminSession(row interface{ Scan(...any) error }) (AdminSession, error) {
	var a AdminSession
	var created, seen, expires int64
	if err := row.Scan(&a.TokenHash, &a.PublicID, &a.CSRF, &created, &seen, &expires, &a.IP, &a.UserAgent); err != nil {
		return AdminSession{}, err
	}
	a.CreatedAt, a.SeenAt, a.ExpiresAt = time.Unix(created, 0), time.Unix(seen, 0), time.Unix(expires, 0)
	return a, nil
}

func (s *Store) TouchAdminSession(ctx context.Context, tokenHash []byte, seen time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE admin_sessions SET seen_at = ? WHERE token_hash = ?`, seen.Unix(), tokenHash)
	return err
}

func (s *Store) DeleteAdminSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteAdminSessionByPublicID(ctx context.Context, publicID string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE public_id = ?`, publicID))
}

func (s *Store) DeleteAllAdminSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions`)
	return err
}

// PruneAdminSessions deletes sessions idle since before idleBefore or past
// their expiry.
func (s *Store) PruneAdminSessions(ctx context.Context, now, idleBefore time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at <= ? OR seen_at < ?`, now.Unix(), idleBefore.Unix())
	return err
}

// AuditEntry is one admin action.
type AuditEntry struct {
	ID     int64
	At     time.Time
	IP     string
	Action string
	Detail string
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (at, ip, action, detail) VALUES (?, ?, ?, ?)`, e.At.UnixMilli(), e.IP, e.Action, e.Detail)
	return err
}

// AuditLog returns up to limit entries older than beforeID (0 = newest).
func (s *Store) AuditLog(ctx context.Context, beforeID int64, limit int) ([]AuditEntry, error) {
	query := `SELECT id, at, ip, action, detail FROM audit ORDER BY id DESC LIMIT ?`
	args := []any{limit}
	if beforeID > 0 {
		query = `SELECT id, at, ip, action, detail FROM audit WHERE id < ? ORDER BY id DESC LIMIT ?`
		args = []any{beforeID, limit}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.IP, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
