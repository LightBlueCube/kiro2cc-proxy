package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
)

// start plans the attempt and returns a running turn:
//
// The snapshot comes from the request's anchors, else from the user's own
// snapshots.
//
//	snapshot prefix hit ─┬─ parked turn answered by the delta ──▶ continue
//	                     └─ else (cancel what runs there, rewind
//	                        to a rewindable position) ──────────▶ warm delta
//	no hit, or warm failed ─────────────────────────────────────▶ fresh replay
func (g *Gateway) start(ctx context.Context, r *run) (*activeTurn, *attemptFailure) {
	if r.reuse {
		snap, ok := g.anchoredSnapshot(r.t)
		if !ok {
			snap, ok = g.snaps.lookup(r.user, r.t)
		}
		if ok && snap.ToolsKey == toolsKey(r.req.Tools) {
			if at := g.tryContinue(snap, r); at != nil {
				r.mode = ModeContinue
				return at, nil
			}
			at, err := g.startWarm(ctx, snap, r)
			if err == nil {
				return at, nil
			}
			if ctx.Err() != nil {
				return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: ctx.Err().Error()}}
			}
			g.log.Info("warm session unusable; replaying fresh", "session", snap.SessionID, "err", err)
		}
	}
	return g.startFresh(ctx, r)
}

// tryContinue hands the client's tool results to a parked turn whose
// boundary snapshot is the request's prefix.
func (g *Gateway) tryContinue(snap snapshot, r *run) *activeTurn {
	g.mu.Lock()
	defer g.mu.Unlock()
	sess := g.sessions[sessionRef(snap.Account, snap.SessionID)]
	if sess == nil || sess.turn == nil {
		return nil
	}
	at := sess.turn
	sameTurn := at.parked != nil && !at.finished && at.choice == r.choice() && snap.SystemHash == hashString(r.t.system)
	if !sameTurn {
		return nil
	}
	results, trailing, ok := matchResults(at.parked, r.t.body[snap.Count:])
	if !ok {
		return nil
	}
	group := at.parked
	at.parked = nil
	at.parkTimer.Stop()
	at.consuming = true
	group.settle(groupResult(group, results, trailing), nil)
	return at
}

// startWarm prompts the snapshot's session with the transcript delta.
func (g *Gateway) startWarm(ctx context.Context, snap snapshot, r *run) (*activeTurn, error) {
	account := g.pool.Account(snap.Account)
	if account == nil {
		return nil, fmt.Errorf("account %s is no longer configured", snap.Account)
	}
	if err := account.Available(r.group); err != nil {
		return nil, err
	}
	at, err := g.claimSession(ctx, snap, account, r.req.Tools)
	if err != nil {
		if !errors.Is(err, errSessionBusy) && ctx.Err() == nil {
			g.forget(snap.Account, snap.SessionID)
		}
		return nil, err
	}
	r.mode = ModeWarm
	if snap.Steps > 0 {
		rewound, err := g.rewindTo(ctx, at, snap, r.req.Tools)
		if err != nil {
			g.releaseClaim(at)
			if ctx.Err() == nil {
				g.forget(snap.Account, snap.SessionID)
			}
			return nil, err
		}
		if rewound {
			r.mode = ModeRewind
		}
	}
	if err := account.TryAcquire(r.group); err != nil {
		g.releaseClaim(at)
		return nil, err
	}
	systemUpdate := ""
	if snap.SystemHash != hashString(r.t.system) {
		systemUpdate = r.t.system
		if systemUpdate == "" {
			systemUpdate = "(The client removed its system prompt.)"
		}
	}
	g.launch(at, account, r, deltaPromptBlocks(r.t.body[snap.Count:], systemUpdate), snap.Steps)
	return at, nil
}

// rewindTo returns the claimed session to snap's position when turns ran
// past it (later replies, or what a cancelled turn left), and reports
// whether it did.
func (g *Gateway) rewindTo(ctx context.Context, at *activeTurn, snap snapshot, tools []conv.ToolSpec) (bool, error) {
	g.mu.Lock()
	proc, binding := at.sess.proc, at.sess.binding
	g.mu.Unlock()
	steps, err := proc.StepCount(ctx, snap.SessionID, snap.Steps)
	if err != nil {
		return false, err
	}
	if steps == snap.Steps {
		return false, nil
	}
	if err := proc.Rewind(ctx, snap.SessionID, snap.Steps, sessionSpec(at.sess.account, binding, tools)); err != nil {
		return false, err
	}
	g.log.Info("rewound session", "session", snap.SessionID, "steps", steps, "kept", snap.Steps)
	if err := g.snaps.rewound(snap.Account, snap.SessionID, snap.Count); err != nil {
		g.log.Warn("snapshot rewind not persisted", "err", err)
	}
	return true, nil
}

// claimSession reserves the snapshot's session for a new turn, cancelling
// the turn that runs there, and reopens the session when this process no
// longer holds it.
func (g *Gateway) claimSession(ctx context.Context, snap snapshot, account *pool.Account, tools []conv.ToolSpec) (*activeTurn, error) {
	ref := sessionRef(snap.Account, snap.SessionID)
	for {
		g.mu.Lock()
		sess := g.sessions[ref]
		if sess != nil && sess.turn != nil {
			old := sess.turn
			g.mu.Unlock()
			if old.closing {
				// Being closed for idleness: wait, then reopen it.
				select {
				case <-old.closed:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if old.turn == nil {
				return nil, errSessionBusy
			}
			old.turn.Cancel()
			select {
			case <-old.closed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		held := sess != nil && sess.proc.Alive() && sess.proc.HasSession(snap.SessionID)
		live := held && !sess.proc.Retiring()
		if sess == nil {
			sess = &session{ref: ref, id: snap.SessionID, user: snap.User, account: account}
			g.sessions[ref] = sess
		}
		at := newActiveTurn(sess)
		sess.turn = at
		g.mu.Unlock()
		if live {
			return at, nil
		}
		var err error
		if held {
			// A retiring process (the process mode changed) still holds
			// it; one process at a time may hold a session.
			if err = sess.proc.CloseSession(ctx, snap.SessionID); err != nil {
				err = fmt.Errorf("close session in retiring process: %w", err)
			}
		}
		if err == nil {
			err = g.reopen(ctx, sess, tools)
		}
		if err != nil {
			g.mu.Lock()
			if sess.binding != nil {
				sess.binding.Close()
			}
			delete(g.sessions, ref)
			g.mu.Unlock()
			return nil, err
		}
		return at, nil
	}
}

// reopen restores a persisted session into the account's current process.
func (g *Gateway) reopen(ctx context.Context, sess *session, tools []conv.ToolSpec) error {
	if _, err := os.Stat(sess.account.ConversationStore(sess.id)); err != nil {
		return fmt.Errorf("conversation store: %w", err)
	}
	proc, err := sess.account.Proc(context.WithoutCancel(ctx), profileFor(tools))
	if err != nil {
		return err
	}
	binding, err := g.bridge.Bind(bridgeTools(tools), sessionTools{g: g, sess: sess})
	if err != nil {
		return err
	}
	if err := proc.ReopenSession(ctx, sess.id, sessionSpec(sess.account, binding, tools)); err != nil {
		binding.Close()
		return err
	}
	g.mu.Lock()
	if sess.binding != nil {
		sess.binding.Close()
	}
	sess.proc, sess.binding, sess.known = proc, binding, toolNames(tools)
	g.mu.Unlock()
	return nil
}

// startFresh opens a new session on the best account and replays the
// transcript. Accounts that fail to open one are skipped.
func (g *Gateway) startFresh(ctx context.Context, r *run) (*activeTurn, *attemptFailure) {
	var lastErr error
	for {
		account, err := g.pool.Pick(r.user, r.group, r.exclude)
		if err != nil {
			message := err.Error()
			if lastErr != nil {
				message += "; last error: " + lastErr.Error()
			}
			return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUnavailable, Message: message}}
		}
		at, err := g.openSession(ctx, r, account)
		if err == nil {
			r.mode = ModeFresh
			g.launch(at, account, r, freshPrompt(r.t, replayBudget), 0)
			return at, nil
		}
		account.Release()
		if ctx.Err() != nil {
			return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUpstream, Message: ctx.Err().Error()}}
		}
		if agy.IsAuthError(err) {
			account.Disable(err)
		}
		g.log.Warn("account could not open a session", "account", account.Name(), "err", err)
		r.exclude[account.Name()] = true
		lastErr = err
	}
}

// openSession creates a session on account and claims it.
func (g *Gateway) openSession(ctx context.Context, r *run, account *pool.Account) (*activeTurn, error) {
	proc, err := account.Proc(context.WithoutCancel(ctx), profileFor(r.req.Tools))
	if err != nil {
		return nil, err
	}
	sess := &session{user: r.user, account: account, proc: proc, known: toolNames(r.req.Tools)}
	binding, err := g.bridge.Bind(bridgeTools(r.req.Tools), sessionTools{g: g, sess: sess})
	if err != nil {
		return nil, err
	}
	id, models, err := proc.NewSession(ctx, sessionSpec(account, binding, r.req.Tools))
	if err != nil {
		binding.Close()
		return nil, err
	}
	g.models.update(models)
	sess.id, sess.ref, sess.binding = id, sessionRef(account.Name(), id), binding
	at := newActiveTurn(sess)
	g.mu.Lock()
	sess.turn = at
	g.sessions[sess.ref] = sess
	g.mu.Unlock()
	return at, nil
}

// launch starts the prompt on a claimed session that has at least
// baseSteps native steps. The slot on account is held until the turn
// settles.
func (g *Gateway) launch(at *activeTurn, account *pool.Account, r *run, prompt []agy.ContentBlock, baseSteps int) {
	g.mu.Lock()
	at.account, at.choice, at.consuming, at.baseSteps, at.started = account, r.choice(), true, baseSteps, time.Now()
	at.turn = at.sess.proc.RunTurn(agy.TurnSpec{SessionID: at.sess.id, Model: r.slug, Prompt: prompt})
	at.sess.lastUsed = time.Now()
	g.mu.Unlock()
	go g.watch(at)
}

// releaseClaim gives up a claim that never launched.
func (g *Gateway) releaseClaim(at *activeTurn) {
	g.mu.Lock()
	if at.sess.turn == at {
		at.sess.turn = nil
	}
	g.mu.Unlock()
}

func sessionSpec(account *pool.Account, binding *bridge.Binding, tools []conv.ToolSpec) agy.SessionSpec {
	return agy.SessionSpec{
		Cwd: account.Workspace(),
		MCPServers: []agy.MCPServer{{
			Name:    agy.BridgeServer,
			Type:    "http",
			URL:     binding.URL(),
			Headers: []agy.HTTPHeader{{Name: bridge.TokenHeader, Value: binding.Token()}},
		}},
		SystemAppend: toolPolicy(tools),
	}
}

func toolNames(tools []conv.ToolSpec) map[string]bool {
	known := make(map[string]bool, len(tools))
	for _, tool := range tools {
		known[tool.Name] = true
	}
	return known
}
