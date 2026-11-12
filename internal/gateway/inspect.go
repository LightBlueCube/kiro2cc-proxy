package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SessionInfo describes an ACP session this process knows.
type SessionInfo struct {
	Account string
	ID      string
	// KeyHash is the API key whose conversation the session caches.
	KeyHash string
	// PID is the agy process holding the session (0 when not open).
	PID      int
	LastUsed time.Time
	// Turn is the running or parked turn, nil when idle.
	Turn *TurnInfo
}

// TurnState says what a turn is doing.
type TurnState string

const (
	TurnStarting TurnState = "starting"
	TurnRunning  TurnState = "running"
	// TurnParked waits for the client's tool results.
	TurnParked TurnState = "waiting for tool results"
	// TurnDetached runs with no client reading it.
	TurnDetached TurnState = "running unobserved"
	// TurnClosing is no turn: the idle session is being closed.
	TurnClosing TurnState = "closing"
)

type TurnInfo struct {
	State TurnState
	// Model is the model as the client named it.
	Model    string
	Started  time.Time
	ParkedAt time.Time
	// ParkTimeout is when a parked turn is cancelled.
	ParkTimeout time.Time
}

// Sessions lists the known sessions, busiest first.
func (g *Gateway) Sessions() []SessionInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]SessionInfo, 0, len(g.sessions))
	for _, sess := range g.sessions {
		info := SessionInfo{Account: sess.account.Name(), ID: sess.id, KeyHash: sess.user, LastUsed: sess.lastUsed}
		if sess.proc != nil && sess.proc.Alive() && sess.proc.HasSession(sess.id) {
			info.PID = sess.proc.PID()
		}
		if at := sess.turn; at != nil {
			info.Turn = turnInfo(at)
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b SessionInfo) int {
		if (a.Turn != nil) != (b.Turn != nil) {
			if a.Turn != nil {
				return -1
			}
			return 1
		}
		return b.LastUsed.Compare(a.LastUsed)
	})
	return out
}

// turnInfo needs g.mu.
func turnInfo(at *activeTurn) *TurnInfo {
	model, _, _ := strings.Cut(at.choice, "\x00")
	info := &TurnInfo{Model: model, Started: at.started}
	switch {
	case at.closing:
		info.State = TurnClosing
	case at.turn == nil:
		info.State = TurnStarting
	case at.parked != nil:
		info.State, info.ParkedAt, info.ParkTimeout = TurnParked, at.parkedAt, at.parkedAt.Add(parkTTL)
	case at.consuming:
		info.State = TurnRunning
	default:
		info.State = TurnDetached
	}
	return info
}

// ErrNoSession means the session is not known or not in the state the
// action needs.
var ErrNoSession = errors.New("no such session")

// CancelTurn cancels the session's running or parked turn. Its client
// gets an error; the session stays reusable up to the cancelled turn.
func (g *Gateway) CancelTurn(account, sessionID string) error {
	g.mu.Lock()
	sess := g.sessions[sessionRef(account, sessionID)]
	if sess == nil || sess.turn == nil || sess.turn.turn == nil {
		g.mu.Unlock()
		return fmt.Errorf("%w with a running turn", ErrNoSession)
	}
	turn := sess.turn.turn
	g.mu.Unlock()
	turn.Cancel()
	g.log.Info("turn cancelled by the admin", "account", account, "session", sessionID)
	return nil
}

// CloseSession closes an idle session in agy, which under
// --process-per-session ends its worker. Its snapshot stays: the next
// request of that conversation reopens the session from disk.
func (g *Gateway) CloseSession(ctx context.Context, account, sessionID string) error {
	g.mu.Lock()
	sess := g.sessions[sessionRef(account, sessionID)]
	if sess == nil || sess.turn != nil || !sess.proc.Alive() || !sess.proc.HasSession(sessionID) {
		g.mu.Unlock()
		return fmt.Errorf("%w that is open and idle", ErrNoSession)
	}
	claim := g.claimClosingLocked(sess)
	g.mu.Unlock()
	if err := g.closeClaimed(ctx, claim); err != nil {
		return err
	}
	g.log.Info("session closed by the admin", "account", account, "session", sessionID)
	return nil
}
