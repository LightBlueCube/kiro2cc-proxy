package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"
)

// UpstreamRetention is how long captured upstream calls are kept.
const UpstreamRetention = 24 * time.Hour

// maxUpstreamBody bounds one decompressed body; a gzip bomb in the
// database cannot grow it further.
const maxUpstreamBody = 512 << 20

// UpstreamCall is one Cloud Code generation call agy made, as sent and
// as answered.
type UpstreamCall struct {
	ID int64
	At time.Time
	// CallToken ties the call to the request (Request.CallToken) it served.
	CallToken   string
	Account     string
	AccountName string
	// Session is the ACP session the call served.
	Session string
	// Seq is the call's number within its trajectory.
	Seq    int
	Model  string
	Status int
	// Request and Response are the bodies as they crossed the wire.
	Request  []byte
	Response []byte
	// ResponseTruncated is set when Response lost its end to the capture
	// limit.
	ResponseTruncated bool
}

// UpstreamCallInfo is an UpstreamCall without its bodies.
type UpstreamCallInfo struct {
	ID                int64
	At                time.Time
	Account           string
	AccountName       string
	Session           string
	Seq               int
	Model             string
	Status            int
	RequestBytes      int64
	ResponseBytes     int64
	ResponseTruncated bool
}

func upstreamCutoff() int64 { return time.Now().Add(-UpstreamRetention).UnixMilli() }

const upstreamAccountNameExpr = `COALESCE((SELECT name FROM accounts WHERE id = upstream_calls.account), NULLIF(account_name, ''), account)`

// AddUpstreamCall stores the call with its bodies compressed.
func (s *Store) AddUpstreamCall(ctx context.Context, c UpstreamCall) error {
	request, err := compress(c.Request)
	if err != nil {
		return err
	}
	response, err := compress(c.Response)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO upstream_calls (at, call_token, account, account_name, session, seq, model, status, request, response,
		request_bytes, response_bytes, response_truncated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.At.UnixMilli(), c.CallToken, c.Account, c.AccountName, c.Session, c.Seq, c.Model, c.Status, request, response,
		len(c.Request), len(c.Response), c.ResponseTruncated)
	return err
}

// UpstreamCalls lists the kept calls captured for the request with the
// id, oldest first, without their bodies.
func (s *Store) UpstreamCalls(ctx context.Context, requestID int64) ([]UpstreamCallInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, account, `+upstreamAccountNameExpr+`, session, seq, model, status, request_bytes, response_bytes, response_truncated
		FROM upstream_calls
		WHERE call_token != '' AND call_token = (SELECT call_token FROM requests WHERE id = ?) AND at >= ? ORDER BY id`, requestID, upstreamCutoff())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamCallInfo
	for rows.Next() {
		var c UpstreamCallInfo
		var at int64
		if err := rows.Scan(&c.ID, &at, &c.Account, &c.AccountName, &c.Session, &c.Seq, &c.Model, &c.Status, &c.RequestBytes, &c.ResponseBytes, &c.ResponseTruncated); err != nil {
			return nil, err
		}
		c.At = time.UnixMilli(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpstreamCall returns the kept call with its bodies, or ErrNotFound.
func (s *Store) UpstreamCall(ctx context.Context, id int64) (UpstreamCall, error) {
	var c UpstreamCall
	var at int64
	var request, response []byte
	err := s.db.QueryRowContext(ctx, `SELECT id, at, call_token, account, `+upstreamAccountNameExpr+`, session, seq, model, status, request, response, response_truncated
		FROM upstream_calls WHERE id = ? AND at >= ?`, id, upstreamCutoff()).
		Scan(&c.ID, &at, &c.CallToken, &c.Account, &c.AccountName, &c.Session, &c.Seq, &c.Model, &c.Status, &request, &response, &c.ResponseTruncated)
	if errors.Is(err, sql.ErrNoRows) {
		return UpstreamCall{}, ErrNotFound
	}
	if err != nil {
		return UpstreamCall{}, err
	}
	c.At = time.UnixMilli(at)
	if c.Request, err = decompress(request); err != nil {
		return UpstreamCall{}, fmt.Errorf("upstream call %d request: %w", id, err)
	}
	if c.Response, err = decompress(response); err != nil {
		return UpstreamCall{}, fmt.Errorf("upstream call %d response: %w", id, err)
	}
	return c, nil
}

// PruneUpstream deletes the calls older than UpstreamRetention.
func (s *Store) PruneUpstream(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM upstream_calls WHERE at < ?`, upstreamCutoff())
	return err
}

// DeleteUpstreamCalls deletes every captured call.
func (s *Store) DeleteUpstreamCalls(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM upstream_calls`)
	return err
}

func compress(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompress(packed []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, maxUpstreamBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxUpstreamBody {
		return nil, fmt.Errorf("body exceeds %d bytes", maxUpstreamBody)
	}
	return raw, nil
}
