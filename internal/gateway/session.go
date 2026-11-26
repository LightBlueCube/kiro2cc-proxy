package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
)

func cancelledFailure() *attemptFailure {
	return &attemptFailure{err: &conv.Error{Kind: conv.ErrCancelled, Message: "client disconnected"}}
}

// start plans the attempt and returns a running turn:
//
// The snapshot comes from the request's anchors, else from the user's own
// snapshots.
//
//	snapshot prefix hit ─┬─ account unavailable / lacks model ──▶ account failure
//	                     ├─ parked turn answered by the delta ──▶ continue
//	                     └─ else (cancel what runs there, rewind
//	                        to a rewindable position) ──────────▶ warm delta
//	no hit ─────────────────────────────────────────────────────▶ fresh replay
//
// Input past a position that cannot be rewound to removes it, so a
// cancelled turn's input is never sent again on top of itself: the next
// request plans from an earlier rewindable position.
func (g *Gateway) start(ctx context.Context, r *run) (*activeTurn, *attemptFailure) {
	if ctx.Err() != nil {
		return nil, cancelledFailure()
	}
	if !r.reuse {
		return g.startFresh(ctx, r, r.moveReason)
	}
	for {
		if ctx.Err() != nil {
			return nil, cancelledFailure()
		}
		snap, ok := g.anchoredSnapshot(r.t)
		if !ok {
			snap, ok = g.snaps.lookup(r.user, r.t)
		}
		if !ok {
			return g.startFresh(ctx, r, ReplayNoSnapshot)
		}
		account, slug, tier, acctErr := g.checkAccount(snap.Account, r)
		if acctErr != nil {
			if acctErr.definite {
				g.cancelParked(ctx, snap.Account, snap.SessionID)
			}
			if ctx.Err() != nil {
				return nil, cancelledFailure()
			}
			return nil, g.accountFailure(snap.Account, acctErr)
		}
		if snap.ToolsKey != toolsKey(r.req.Tools) {
			// A new session on the same account: only a failure moves a
			// conversation to another.
			r.pin = snap.Account
			return g.startFresh(ctx, r, ReplayToolsChanged)
		}
		if at := g.tryContinue(ctx, snap, r); at != nil {
			r.mode = ModeContinue
			return at, nil
		}
		at, err := g.startWarm(ctx, snap, account, slug, tier, r)
		if err == nil {
			return at, nil
		}
		if errors.Is(err, errPositionGone) {
			continue
		}
		var limitErr *conv.Error
		if errors.As(err, &limitErr) {
			return nil, &attemptFailure{err: limitErr}
		}
		if ctx.Err() != nil {
			return nil, cancelledFailure()
		}
		return nil, g.warmFailure(snap, err)
	}
}

// cancelParked cancels a turn waiting for client tool results on this session
// and waits for it to release its worker slot, so a retry that switches
// accounts does not collide with key/global worker limits. Running turns are
// left alone.
func (g *Gateway) cancelParked(ctx context.Context, account, sessionID string) {
	g.mu.Lock()
	sess := g.sessions[sessionRef(account, sessionID)]
	var old *activeTurn
	if sess != nil && sess.turn != nil && sess.turn.parked != nil && sess.turn.turn != nil {
		old = sess.turn
	}
	g.mu.Unlock()
	if old == nil {
		return
	}
	old.turn.Cancel()
	select {
	case <-old.closed:
	case <-ctx.Done():
	}
}

// warmFailure tells the client that the account holding its conversation's
// session cannot resume it. The conversation does not replay on another
// account unasked: that loses the reasoning.
func (g *Gateway) warmFailure(snap snapshot, err error) *attemptFailure {
	var accountErr *accountError
	if !errors.As(err, &accountErr) {
		accountErr = newAccountError("could not resume the conversation's session", false, err)
	}
	return g.accountFailure(snap.Account, accountErr)
}

// accountFailure is a failure of the account to serve the conversation.
func (g *Gateway) accountFailure(id string, err *accountError) *attemptFailure {
	name := id
	if account := g.pool.Account(id); account != nil {
		name = account.Name()
	}
	g.log.Warn("account could not serve the conversation", "account", name, "definite", err.definite, "wait", err.wait, "err", err)
	if err.wait > 0 {
		secs := int(math.Ceil(err.wait.Seconds()))
		return &attemptFailure{
			err:         &conv.Error{Kind: conv.ErrQuota, Message: fmt.Sprintf("rate limit exceeded; retry in %d s", secs), RetryAfterSeconds: secs},
			account:     id,
			accountName: name,
		}
	}
	return &attemptFailure{
		err:         &conv.Error{Kind: conv.ErrUpstream, Message: err.reason},
		account:     id,
		accountName: name,
		retry:       true,
		definite:    err.definite,
	}
}

// tryContinue hands the client's tool results (and any trailing user message)
// to a parked turn whose boundary snapshot is the request's prefix.
func (g *Gateway) tryContinue(ctx context.Context, snap snapshot, r *run) *activeTurn {
	g.mu.Lock()
	if ctx.Err() != nil {
		g.mu.Unlock()
		return nil
	}
	sess := g.sessions[sessionRef(snap.Account, snap.SessionID)]
	if sess == nil || sess.turn == nil {
		g.mu.Unlock()
		return nil
	}
	at := sess.turn
	sameTurn := at.parked != nil && !at.finished && at.choice == r.choice()
	if sameTurn {
		select {
		case <-at.turn.Done():
			sameTurn = false
		default:
		}
	}
	if !sameTurn {
		g.mu.Unlock()
		return nil
	}
	results, trailing, ok := matchResults(at.parked, r.t.body[snap.Count:])
	if !ok {
		g.mu.Unlock()
		return nil
	}
	group := at.parked
	at.parked = nil
	at.account.BeginRun(at, time.Now())
	at.parkTimer.Stop()
	at.consuming = true
	g.mu.Unlock()

	g.movedPast(snap.Account, snap.SessionID)
	sysNotes, steer := splitTrailing(trailing)
	if len(steer) > 0 {
		if err := at.turn.Steer(ctx, deltaPromptBlocks(steer)); err != nil {
			if ctx.Err() == nil {
				g.mu.Lock()
				at.steerErr = err
				g.mu.Unlock()
			}
			return at
		}
	}
	if ctx.Err() != nil {
		return at
	}
	group.settle(groupResult(group, results, sysNotes), nil)
	return at
}

// startWarm prompts the snapshot's session with the transcript delta.
func (g *Gateway) startWarm(ctx context.Context, snap snapshot, account *pool.Account, slug, tier string, r *run) (*activeTurn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	at, err := g.claimSession(ctx, snap, account, r)
	if err != nil {
		return nil, g.claimFailure(ctx, snap, account, r, err)
	}
	// Claiming may have waited for a cancelled turn that took input past
	// snap.
	if _, ok := g.snaps.position(snap.Account, snap.SessionID, snap.Count, snap.Hash); !ok {
		g.releaseClaim(at)
		return nil, errPositionGone
	}
	r.mode = ModeWarm
	if snap.Steps > 0 {
		if err := account.Available(r.group); err != nil {
			g.releaseClaim(at)
			return nil, unavailableError(account, r, err)
		}
		rewound, err := g.rewindTo(ctx, at, snap, r)
		if err != nil {
			g.releaseClaim(at)
			if ctx.Err() != nil {
				return nil, err
			}
			return nil, newAccountError("could not rewind the conversation's session", false, err)
		}
		if rewound {
			r.mode = ModeRewind
		}
	}
	if err := account.Available(r.group); err != nil {
		g.releaseClaim(at)
		return nil, unavailableError(account, r, err)
	}
	if err := ctx.Err(); err != nil {
		g.releaseClaim(at)
		return nil, err
	}
	g.launch(at, account, r, slug, tier, deltaPromptBlocks(r.t.body[snap.Count:]), snap.Steps)
	return at, nil
}

// rewindTo returns the claimed session to snap's position when turns ran
// past it (later replies, or what a cancelled turn left), and reports
// whether it did.
func (g *Gateway) rewindTo(ctx context.Context, at *activeTurn, snap snapshot, r *run) (bool, error) {
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
	if err := proc.Rewind(ctx, snap.SessionID, snap.Steps, sessionSpec(at.sess.account, binding, r)); err != nil {
		return false, err
	}
	g.log.Info("rewound session", "session", snap.SessionID, "steps", steps, "kept", snap.Steps)
	if err := g.snaps.rewound(snap.Account, snap.SessionID, snap.Count); err != nil {
		g.log.Warn("snapshot rewind not persisted", "err", err)
	}
	return true, nil
}

// claimFailure classifies why claimSession could not give the snapshot's
// session to the request. A session lost on its account is forgotten: no
// retry brings it back.
func (g *Gateway) claimFailure(ctx context.Context, snap snapshot, account *pool.Account, r *run, err error) error {
	var limitErr *conv.Error
	var accountErr *accountError
	switch {
	case errors.As(err, &limitErr), ctx.Err() != nil:
		return err
	case errors.Is(err, errSessionBusy), errors.Is(err, pool.ErrSlotBusy):
		return newAccountError("the conversation's session is busy", false, err)
	case errors.As(err, &accountErr):
		if accountErr.definite {
			g.forget(snap.Account, snap.SessionID)
		}
		return err
	}
	if availErr := account.Available(r.group); availErr != nil {
		return unavailableError(account, r, availErr)
	}
	return newAccountError("could not reopen the conversation's session", false, err)
}

// claimSession reserves the snapshot's session for a new turn, cancelling
// the turn that runs there, and reopens the session when this process no
// longer holds it.
func (g *Gateway) claimSession(ctx context.Context, snap snapshot, account *pool.Account, r *run) (*activeTurn, error) {
	ref := sessionRef(snap.Account, snap.SessionID)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
		parked, limitErr := g.tryAcquireBusyOrEvictLocked(r)
		if limitErr != nil {
			g.mu.Unlock()
			return nil, limitErr
		}
		if parked != nil {
			g.mu.Unlock()
			if !g.cancelEvictedParked(ctx, parked) {
				return nil, ctx.Err()
			}
			continue
		}
		if err := account.TryAcquire(r.group); err != nil {
			g.releaseBusyLocked(r.user)
			var evicted *activeTurn
			if r.evictWaiting && errors.Is(err, pool.ErrSlotBusy) {
				evicted = g.evictParkedLocked(r.user, account)
			}
			g.mu.Unlock()
			if evicted == nil {
				return nil, err
			}
			if !g.cancelEvictedParked(ctx, evicted) {
				return nil, ctx.Err()
			}
			continue
		}
		live := sess != nil && sess.proc.Alive() && sess.proc.HasSession(snap.SessionID)
		if sess == nil {
			sess = &session{ref: ref, id: snap.SessionID, account: account}
			g.sessions[ref] = sess
		}
		// The key that claims the session owns it from now on, as the
		// snapshot recorded after the turn will say: the busy slot taken
		// above for r.user is released through sess.user.
		sess.user = r.user
		at := newActiveTurn(sess)
		at.account = account
		sess.turn = at
		evicted := g.evictIdleLocked(r.user, r.maxWorkers, account)
		g.mu.Unlock()
		g.closeEvicted(evicted)
		if live {
			return at, nil
		}
		if err := g.reopen(ctx, sess, r); err != nil {
			g.mu.Lock()
			g.dropSessionLocked(sess)
			g.releaseTurnLocked(at)
			g.mu.Unlock()
			close(at.closed)
			return nil, err
		}
		return at, nil
	}
}

// reopen restores a persisted session into the account's current process.
func (g *Gateway) reopen(ctx context.Context, sess *session, r *run) error {
	if _, err := os.Stat(sess.account.ConversationStore(sess.id)); err != nil {
		return newAccountError("the conversation's session was lost on its account", true, fmt.Errorf("conversation store: %w", err))
	}
	tools := r.req.Tools
	proc, err := sess.account.Proc(ctx, profileFor(tools))
	if err != nil {
		return err
	}
	binding, err := g.bridge.Bind(bridgeTools(tools), sessionTools{g: g, sess: sess})
	if err != nil {
		return err
	}
	if err := proc.ReopenSession(ctx, sess.id, sessionSpec(sess.account, binding, r)); err != nil {
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

// startFresh opens a new session on the best account that offers the
// model and replays the transcript; reason says why, when it has history.
// Accounts that fail to open one are skipped.
func (g *Gateway) startFresh(ctx context.Context, r *run, reason ReplayReason) (*activeTurn, *attemptFailure) {
	if r.pin != "" {
		if failure := g.pinnedFailure(r); failure != nil {
			return nil, failure
		}
	}
	var lacking []string
	for {
		if ctx.Err() != nil {
			return nil, cancelledFailure()
		}
		for _, a := range g.pool.Accounts() {
			if r.exclude[a.ID()] {
				continue
			}
			if _, _, ok := slugOn(a, r); !ok {
				r.exclude[a.ID()] = true
				lacking = append(lacking, a.Name())
			}
		}
		account, err := g.pickAccount(ctx, r)
		if ctx.Err() != nil {
			if account != nil {
				account.Release()
			}
			return nil, cancelledFailure()
		}
		if err != nil && r.pin != "" {
			if failure := g.pinnedFailure(r); failure != nil {
				return nil, failure
			}
			return nil, g.accountFailure(r.pin, newAccountError("the account holding this conversation is busy", false, err))
		}
		if err != nil {
			// Catalogs that sessions just reported may show no account
			// has the model, as the fallback list had promised.
			if _, offered := lookupModel(g.offered(), r.req.Model, r.req.Effort); !offered {
				return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("unknown model %q", r.req.Model)}}
			}
			g.log.Warn("no account available for request", "model", r.req.Model, "err", err, "lacking", lacking)
			if g.hasAvailableAccount(r) {
				return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUnavailable, Message: "service temporarily unavailable"}}
			}
			if wait := g.minShortCooldown(r); wait > 0 {
				secs := int(math.Ceil(wait.Seconds()))
				return nil, &attemptFailure{err: &conv.Error{
					Kind:              conv.ErrQuota,
					Message:           fmt.Sprintf("rate limit exceeded; retry in %d s", secs),
					RetryAfterSeconds: secs,
				}}
			}
			return nil, &attemptFailure{err: &conv.Error{Kind: conv.ErrUnknownModel, Message: fmt.Sprintf("model %q is currently unavailable", r.req.Model)}}
		}
		if _, _, ok := slugOn(account, r); !ok {
			account.Release()
			r.exclude[account.ID()] = true
			lacking = append(lacking, account.Name())
			continue
		}
		at, err := g.openSession(ctx, r, account)
		if err == nil {
			// The new session reported the account's catalog, so this is
			// exact even for an account that had not reported one; the
			// unused session is closed when idle.
			slug, tier, ok := slugOn(account, r)
			if !ok {
				g.releaseClaim(at)
				r.exclude[account.ID()] = true
				lacking = append(lacking, account.Name())
				continue
			}
			if err := account.Available(r.group); err != nil {
				g.releaseClaim(at)
				if r.pin != "" {
					return nil, g.accountFailure(r.pin, unavailableError(account, r, err))
				}
				r.exclude[account.ID()] = true
				continue
			}
			r.mode, r.replayReason = ModeFresh, ""
			if newestInput(r.t.body) > 0 {
				r.mode, r.replayReason = ModeReplay, reason
			}
			if ctx.Err() != nil {
				g.releaseClaim(at)
				return nil, cancelledFailure()
			}
			g.launch(at, account, r, slug, tier, freshPrompt(r.t), 0)
			return at, nil
		}
		account.Release()
		var limitErr *conv.Error
		if errors.As(err, &limitErr) {
			return nil, &attemptFailure{err: limitErr}
		}
		if ctx.Err() != nil {
			return nil, cancelledFailure()
		}
		authFailed := agy.IsAuthError(err)
		if authFailed {
			account.Disable(err)
		}
		return nil, g.accountFailure(account.ID(), newAccountError("could not start a session on the account", authFailed, err))
	}
}

// checkAccount verifies that accountID exists, offers r's model, and can serve
// r.group right now.
func (g *Gateway) checkAccount(accountID string, r *run) (*pool.Account, string, string, *accountError) {
	account := g.pool.Account(accountID)
	if account == nil {
		return nil, "", "", newAccountError("the account holding this conversation was removed", true, fmt.Errorf("account %s is no longer configured", accountID))
	}
	slug, tier, ok := slugOn(account, r)
	if !ok {
		return nil, "", "", newAccountError(fmt.Sprintf("the account holding this conversation does not offer model %q", r.req.Model), true, nil)
	}
	if err := account.Available(r.group); err != nil {
		return nil, "", "", unavailableError(account, r, err)
	}
	return account, slug, tier, nil
}

// pinnedFailure checks the account a conversation is bound to before a new
// session opens there; nil means it can serve the request.
func (g *Gateway) pinnedFailure(r *run) *attemptFailure {
	if _, _, _, err := g.checkAccount(r.pin, r); err != nil {
		return g.accountFailure(r.pin, err)
	}
	return nil
}

// unavailableError is why the account holding a conversation cannot serve
// it now. A cooldown that ends soon is a rate limit the client waits out
// on the account; anything else is the account's, and the conversation
// leaves it.
func unavailableError(account *pool.Account, r *run, cause error) *accountError {
	if wait := account.CooldownRemaining(r.group); wait > 0 && wait <= maxRetryableQuotaWait {
		err := newAccountError("rate limit exceeded", false, cause)
		err.wait = wait
		return err
	}
	return newAccountError("the account holding this conversation is unavailable", true, cause)
}

func (g *Gateway) pickAccount(ctx context.Context, r *run) (*pool.Account, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		account, err := g.tryPickAccount(r)
		if err == nil {
			return account, nil
		}
		evict := g.pickParkedForAccount(r)
		if evict == nil {
			return nil, err
		}
		if !g.cancelEvictedParked(ctx, evict) {
			return nil, ctx.Err()
		}
	}
}

func (g *Gateway) tryPickAccount(r *run) (*pool.Account, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	idle := make(map[string]pool.IdleSessions, len(g.sessions))
	for _, sess := range g.sessions {
		if sess.turn != nil {
			continue
		}
		if !sess.proc.Alive() || !sess.proc.HasSession(sess.id) {
			g.dropSessionLocked(sess)
			continue
		}
		id := sess.account.ID()
		cur := idle[id]
		cur.Count++
		if cur.Count == 1 || sess.lastUsed.Before(cur.Oldest) {
			cur.Oldest = sess.lastUsed
		}
		idle[id] = cur
	}
	exclude := r.exclude
	if r.pin != "" {
		exclude = maps.Clone(r.exclude)
		for _, account := range g.pool.Accounts() {
			if account.ID() != r.pin {
				exclude[account.ID()] = true
			}
		}
	}
	return g.pool.Pick(sessionAffinity(r.user, r.t), r.group, exclude, idle)
}

// sessionAffinity keys rendezvous account selection by API key and the
// conversation's opening prefix (system prompt + first message), so one key's
// distinct sessions spread across accounts while replays of the same
// conversation stay deterministic.
func sessionAffinity(user string, t *transcript) string {
	return user + "\x00" + t.hashes[1]
}

// openSession creates a session on account and claims it.
func (g *Gateway) openSession(ctx context.Context, r *run, account *pool.Account) (*activeTurn, error) {
	var evicted []*activeTurn
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.mu.Lock()
		parked, err := g.tryAcquireBusyOrEvictLocked(r)
		if err != nil {
			g.mu.Unlock()
			return nil, err
		}
		if parked != nil {
			g.mu.Unlock()
			if !g.cancelEvictedParked(ctx, parked) {
				return nil, ctx.Err()
			}
			continue
		}
		evicted = g.evictIdleLocked(r.user, r.maxWorkers, account)
		g.mu.Unlock()
		break
	}
	g.closeEvicted(evicted)

	proc, err := account.Proc(ctx, profileFor(r.req.Tools))
	if err != nil {
		g.releaseBusy(r.user)
		return nil, err
	}
	sess := &session{user: r.user, account: account, proc: proc, known: toolNames(r.req.Tools)}
	binding, err := g.bridge.Bind(bridgeTools(r.req.Tools), sessionTools{g: g, sess: sess})
	if err != nil {
		g.releaseBusy(r.user)
		return nil, err
	}
	id, models, err := proc.NewSession(ctx, sessionSpec(account, binding, r))
	if err != nil {
		binding.Close()
		g.releaseBusy(r.user)
		return nil, err
	}
	account.SetModels(models)
	sess.id, sess.ref, sess.binding = id, sessionRef(account.ID(), id), binding
	at := newActiveTurn(sess)
	at.account = account
	g.mu.Lock()
	sess.turn = at
	g.sessions[sess.ref] = sess
	g.mu.Unlock()
	return at, nil
}

// launch starts the prompt with the model slug (effort tier) on a claimed
// session that has at least baseSteps native steps. The slot on account is
// held until the turn settles.
func (g *Gateway) launch(at *activeTurn, account *pool.Account, r *run, slug, tier string, prompt []agy.ContentBlock, baseSteps int) {
	g.movedPast(account.ID(), at.sess.id)
	// A failure left by an earlier turn of the session is not this turn's.
	account.ForgetModelFailure(at.sess.id)
	g.mu.Lock()
	at.account, at.choice, at.slug, at.tier, at.consuming, at.baseSteps, at.started = account, r.choice(), slug, tier, true, baseSteps, time.Now()
	at.usageSteps = baseSteps
	account.BeginRun(at, at.started)
	at.turn = at.sess.proc.RunTurn(agy.TurnSpec{SessionID: at.sess.id, Model: slug, Prompt: prompt})
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
	g.releaseTurnLocked(at)
	g.mu.Unlock()
	close(at.closed)
}

// tryAcquireBusyOrEvictLocked reserves one active-turn slot for r's key and
// the global pool. When busy turns fill either cap and r.evictWaiting is set,
// it unparks r's key's oldest parked turn for the caller to cancel; otherwise
// it returns the limit error. It needs g.mu.
func (g *Gateway) tryAcquireBusyOrEvictLocked(r *run) (*activeTurn, *conv.Error) {
	if r.maxWorkers != nil && g.busyByKey[r.user] >= *r.maxWorkers {
		if r.evictWaiting {
			if old := g.evictParkedLocked(r.user, nil); old != nil {
				return old, nil
			}
		}
		return nil, &conv.Error{
			Kind:    conv.ErrKeyBusy,
			Message: fmt.Sprintf("this API key reached its limit of %d concurrent requests", *r.maxWorkers),
		}
	}
	if max := int(g.maxWorkers.Load()); max > 0 && g.busyTotal >= max {
		if r.evictWaiting {
			if old := g.evictParkedLocked(r.user, nil); old != nil {
				return old, nil
			}
		}
		return nil, &conv.Error{
			Kind:    conv.ErrUnavailable,
			Message: "service is busy, please try again later",
		}
	}
	g.busyTotal++
	g.busyByKey[r.user]++
	return nil, nil
}

func olderParked(a, b *activeTurn) bool {
	if b == nil {
		return true
	}
	return a.parkedAt.Before(b.parkedAt) || (a.parkedAt.Equal(b.parkedAt) && a.sess.ref < b.sess.ref)
}

func unparkForEvictionLocked(at *activeTurn) *activeTurn {
	if at == nil {
		return nil
	}
	at.parked = nil
	if at.parkTimer != nil {
		at.parkTimer.Stop()
	}
	return at
}

// evictParkedLocked selects and unparks user's oldest parked turn (restricted
// to account when non-nil) so the caller can cancel it outside g.mu. It needs
// g.mu.
func (g *Gateway) evictParkedLocked(user string, account *pool.Account) *activeTurn {
	var victim *activeTurn
	for _, sess := range g.sessions {
		at := sess.turn
		if at == nil || at.parked == nil || at.turn == nil || at.finished || sess.user != user {
			continue
		}
		if account != nil && sess.account != account {
			continue
		}
		if olderParked(at, victim) {
			victim = at
		}
	}
	return unparkForEvictionLocked(victim)
}

// pickParkedForAccount selects and unparks r's key's oldest parked turn on the
// highest-priority eligible account when r.evictWaiting is enabled.
func (g *Gateway) pickParkedForAccount(r *run) *activeTurn {
	if !r.evictWaiting {
		return nil
	}
	eligible := map[string]int{} // accountID → priority
	for _, a := range g.pool.Accounts() {
		if r.exclude[a.ID()] || (r.pin != "" && a.ID() != r.pin) {
			continue
		}
		if a.Available(r.group) == nil {
			eligible[a.ID()] = a.Priority()
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var victim *activeTurn
	victimPrio := 0
	for _, sess := range g.sessions {
		at := sess.turn
		if at == nil || at.parked == nil || at.turn == nil || at.finished || sess.user != r.user {
			continue
		}
		prio, ok := eligible[sess.account.ID()]
		if !ok {
			continue
		}
		if victim == nil || prio > victimPrio || (prio == victimPrio && olderParked(at, victim)) {
			victim, victimPrio = at, prio
		}
	}
	return unparkForEvictionLocked(victim)
}

// cancelEvictedParked cancels an evicted parked turn, drops its unrewindable
// tool-call snapshot, and waits for its worker slot to be freed.
func (g *Gateway) cancelEvictedParked(ctx context.Context, old *activeTurn) bool {
	g.movedPast(old.account.ID(), old.sess.id)
	g.log.Info("evicting parked turn for worker limit", "account", old.account.Name(), "session", old.sess.id)
	old.turn.Cancel()
	select {
	case <-old.closed:
		return true
	case <-ctx.Done():
		return false
	}
}

func (g *Gateway) releaseTurnLocked(at *activeTurn) {
	g.releaseBusyLocked(at.sess.user)
	at.account.Release()
}

func (g *Gateway) releaseBusyLocked(user string) {
	if g.busyTotal > 0 {
		g.busyTotal--
	}
	if n := g.busyByKey[user] - 1; n > 0 {
		g.busyByKey[user] = n
	} else {
		delete(g.busyByKey, user)
	}
}

func (g *Gateway) releaseBusy(user string) {
	g.mu.Lock()
	g.releaseBusyLocked(user)
	g.mu.Unlock()
}

// evictIdleLocked claims the oldest idle sessions for closing until total
// workers (busy + idle) fit within the key, account, and global caps.
// Passing account == nil checks every account in the pool. It needs g.mu.
func (g *Gateway) evictIdleLocked(user string, keyMax *int, account *pool.Account) []*activeTurn {
	for _, sess := range g.sessions {
		if sess.turn == nil && (!sess.proc.Alive() || !sess.proc.HasSession(sess.id)) {
			g.dropSessionLocked(sess)
		}
	}
	var evicted []*activeTurn
	if keyMax != nil {
		for g.busyByKey[user]+g.idleCountLocked(func(s *session) bool { return s.user == user }) > *keyMax {
			claim := g.evictOldestIdleLocked(func(s *session) bool { return s.user == user })
			if claim == nil {
				break
			}
			evicted = append(evicted, claim)
		}
	}
	if acctMax := g.pool.TurnsPerAccount(); acctMax > 0 {
		accounts := g.pool.Accounts()
		if account != nil {
			accounts = []*pool.Account{account}
		}
		for _, acct := range accounts {
			for acct.Turns()+g.idleCountLocked(func(s *session) bool { return s.account == acct }) > acctMax {
				claim := g.evictOldestIdleLocked(func(s *session) bool { return s.account == acct })
				if claim == nil {
					break
				}
				evicted = append(evicted, claim)
			}
		}
	}
	if globalMax := int(g.maxWorkers.Load()); globalMax > 0 {
		for g.busyTotal+g.idleCountLocked(func(*session) bool { return true }) > globalMax {
			claim := g.evictOldestIdleLocked(func(*session) bool { return true })
			if claim == nil {
				break
			}
			evicted = append(evicted, claim)
		}
	}
	return evicted
}

func (g *Gateway) idleCountLocked(match func(*session) bool) int {
	n := 0
	for _, sess := range g.sessions {
		if sess.turn == nil && match(sess) {
			n++
		}
	}
	return n
}

func (g *Gateway) evictOldestIdleLocked(match func(*session) bool) *activeTurn {
	var oldest *session
	for _, sess := range g.sessions {
		if sess.turn != nil || !match(sess) {
			continue
		}
		if oldest == nil || sess.lastUsed.Before(oldest.lastUsed) || (sess.lastUsed.Equal(oldest.lastUsed) && sess.ref < oldest.ref) {
			oldest = sess
		}
	}
	if oldest == nil {
		return nil
	}
	return g.claimClosingLocked(oldest)
}

func (g *Gateway) closeEvicted(claims []*activeTurn) {
	for _, claim := range claims {
		if err := g.closeClaimed(context.Background(), claim); err != nil {
			g.log.Warn("closing an evicted idle session failed", "session", claim.sess.id, "err", err)
			continue
		}
		g.log.Debug("idle session evicted for worker limit", "account", claim.sess.account.Name(), "session", claim.sess.id)
	}
}

// agy closes its own <System Prompt> tag after the agent profile and puts
// the append behind it; these tags keep the relay's tool policy separate
// from the client's system prompt and mark where the whole prompt ends.
const (
	additionalPromptOpen  = "<Additional System Prompt>"
	additionalPromptClose = "</Additional System Prompt>"
	clientPromptOpen      = "<Client System Prompt>"
	clientPromptClose     = "</Client System Prompt>"
)

// appendedPrompt wraps the tool policy in <Additional System Prompt> and,
// when present, the client's leading system messages verbatim in
// <Client System Prompt>.
func appendedPrompt(tools []conv.ToolSpec, clientSystem string) string {
	policy := additionalPromptOpen + "\n" + toolPolicy(tools) + "\n" + additionalPromptClose
	if clientSystem == "" {
		return policy
	}

	client := clientPromptOpen + "\n" + clientSystem + "\n" + clientPromptClose
	return policy + "\n" + client
}

// sessionSpec appends the appendedPrompt to agy's system prompt. A session
// always gets the same text, since the system prompt seeds its prefix
// hashes: resuming a live session with a different append adds it next to
// the old one (verified on 1.5.3).
func sessionSpec(account *pool.Account, binding *bridge.Binding, r *run) agy.SessionSpec {
	return agy.SessionSpec{
		Cwd: account.Workspace(),
		MCPServers: []agy.MCPServer{{
			Name:    agy.BridgeServer,
			Type:    "http",
			URL:     binding.URL(),
			Headers: []agy.HTTPHeader{{Name: bridge.TokenHeader, Value: binding.Token()}},
		}},
		SystemAppend: appendedPrompt(r.req.Tools, r.t.system),
	}
}

func toolNames(tools []conv.ToolSpec) map[string]bool {
	known := make(map[string]bool, len(tools))
	for _, tool := range tools {
		known[tool.Name] = true
	}
	return known
}
