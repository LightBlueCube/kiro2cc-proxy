package gateway

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
)

const (
	whoAreYouPrompt    = "who are you"
	warmupIdleTimeout  = time.Minute
	warmupCloseTimeout = 10 * time.Second
)

// ErrSlotsBusy reports that worker slots are occupied, so a system call
// deferred without evicting any user session.
var ErrSlotsBusy = errors.New("worker slots busy")

// SystemCall describes one minimal call the relay sends on its own behalf:
// a weekly quota warmup or an administrator's account test.
type SystemCall struct {
	KeyHash string
	KeyName string
	Model   string
	Slug    string
	Effort  string
	Group   string
}

// Warmup sends a minimal prompt on account using req.Slug to start the reset
// countdown of req.Group, records the call via the gateway's Recorder, and
// closes and removes the throwaway session immediately after. It returns
// ErrSlotsBusy without recording a call if all worker slots are occupied.
func (g *Gateway) Warmup(ctx context.Context, account *pool.Account, req SystemCall) error {
	_, err := g.runSystemCall(ctx, account, req)
	return err
}

// Probe makes the same call as Warmup and returns the model's reply.
func (g *Gateway) Probe(ctx context.Context, account *pool.Account, req SystemCall) (string, error) {
	return g.runSystemCall(ctx, account, req)
}

func (g *Gateway) runSystemCall(ctx context.Context, account *pool.Account, req SystemCall) (string, error) {
	if err := g.tryAcquireWarmupSlot(account, req); err != nil {
		return "", err
	}

	rec := Record{
		At:          time.Now(),
		KeyHash:     req.KeyHash,
		KeyName:     req.KeyName,
		Model:       req.Model,
		Slug:        req.Slug,
		Effort:      req.Effort,
		QuotaGroup:  req.Group,
		Account:     account.ID(),
		AccountName: account.Name(),
		CallToken:   rand.Text(),
	}

	proc, err := account.Proc(ctx, IdleProfile())
	if err != nil {
		g.releaseWarmupSlot(account, req.KeyHash)
		g.recordWarmup(rec, time.Time{}, nil, "", warmupContextOrUpstreamErr(ctx, err))
		return "", err
	}

	sessionID, models, err := proc.NewSession(ctx, agy.SessionSpec{Cwd: account.Workspace()})
	if err != nil {
		g.releaseWarmupSlot(account, req.KeyHash)
		if agy.IsAuthError(err) {
			account.Disable(err)
		}
		g.recordWarmup(rec, time.Time{}, nil, "", warmupContextOrUpstreamErr(ctx, err))
		return "", err
	}
	if models != nil {
		account.SetModels(models)
	}
	rec.SessionID = sessionID

	sess := &session{
		ref:      sessionRef(account.ID(), sessionID),
		id:       sessionID,
		user:     req.KeyHash,
		account:  account,
		proc:     proc,
		lastUsed: time.Now(),
	}
	at := newActiveTurn(sess)
	at.account = account
	at.choice = req.Model + "\x00" + req.Effort
	at.slug = req.Slug
	at.tier = req.Effort
	at.consuming = true
	at.started = time.Now()
	at.callToken = rec.CallToken

	account.ForgetModelFailure(sessionID)
	g.mu.Lock()
	sess.turn = at
	g.sessions[sess.ref] = sess
	account.BeginRun(at, at.started)
	at.turn = proc.RunTurn(agy.TurnSpec{
		SessionID:   sessionID,
		Model:       req.Slug,
		Prompt:      []agy.ContentBlock{agy.TextBlock(whoAreYouPrompt)},
		IdleTimeout: warmupIdleTimeout,
	})
	g.mu.Unlock()

	var firstOutput time.Time
	var reply strings.Builder
	for {
		ev, ok := at.turn.Next(ctx)
		if !ok {
			break
		}
		if firstOutput.IsZero() && (ev.Kind == agy.EventText || ev.Kind == agy.EventThought) && ev.Text != "" {
			firstOutput = time.Now()
		}
		if ev.Kind == agy.EventText {
			reply.WriteString(ev.Text)
		}
	}
	if ctx.Err() != nil {
		at.turn.Cancel()
	}
	<-at.turn.Done()

	outcome := at.turn.Outcome()
	failure := account.ModelFailure(sessionID)
	truncated := account.Truncated(sessionID)

	g.mu.Lock()
	risky, stuck, cancelledByAdmin := at.risky, at.stuck, at.cancelledByAdmin
	at.finished = true
	account.EndRun(at)
	g.dropSessionLocked(sess)
	g.releaseTurnLocked(at)
	g.mu.Unlock()
	close(at.closed)

	g.cleanupWarmupSession(account, proc, sessionID)

	var usage *conv.Usage
	var finish conv.FinishReason
	var convErr *conv.Error
	switch {
	case risky:
		convErr = &conv.Error{Kind: conv.ErrRiskControl, Message: "request rejected: Google's risk control refused this request"}
	case stuck != nil:
		convErr = warmupModelFailure(account, req.Group, stuck)
	case outcome.Err != nil:
		if agy.IsAuthError(outcome.Err) {
			account.Disable(outcome.Err)
		}
		convErr = warmupContextOrUpstreamErr(ctx, outcome.Err)
	case outcome.StopReason == agy.StopCancelled:
		msg := "warmup cancelled"
		if cancelledByAdmin {
			msg = "turn cancelled by the administrator"
		}
		convErr = &conv.Error{Kind: conv.ErrCancelled, Message: msg}
	case failure != nil:
		convErr = warmupModelFailure(account, req.Group, failure)
	default:
		if outcome.Usage != nil {
			u := usageFrom(outcome.Usage)
			usage = &u
		}
		finish = conv.FinishStop
		if outcome.StopReason == agy.StopMaxTokens || truncated {
			finish = conv.FinishLength
		}
	}

	g.recordWarmup(rec, firstOutput, usage, finish, convErr)
	if convErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New(convErr.Message)
	}
	return reply.String(), nil
}

func (g *Gateway) tryAcquireWarmupSlot(account *pool.Account, req SystemCall) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var idleTotal, accountIdle int
	for _, sess := range g.sessions {
		if sess.turn != nil {
			continue
		}
		if !sess.proc.Alive() || !sess.proc.HasSession(sess.id) {
			g.dropSessionLocked(sess)
			continue
		}
		idleTotal++
		if sess.account == account {
			accountIdle++
		}
	}
	if max := int(g.maxWorkers.Load()); max > 0 && g.busyTotal+idleTotal >= max {
		return ErrSlotsBusy
	}
	if account.Turns()+accountIdle >= g.pool.TurnsPerAccount() {
		return ErrSlotsBusy
	}
	if err := account.TryAcquire(req.Group); err != nil {
		if errors.Is(err, pool.ErrSlotBusy) {
			return ErrSlotsBusy
		}
		return err
	}
	g.busyTotal++
	g.busyByKey[req.KeyHash]++
	return nil
}

func (g *Gateway) releaseWarmupSlot(account *pool.Account, keyHash string) {
	g.mu.Lock()
	g.releaseBusyLocked(keyHash)
	account.Release()
	g.mu.Unlock()
}

func (g *Gateway) cleanupWarmupSession(account *pool.Account, proc *agy.Proc, sessionID string) {
	closeCtx, cancel := context.WithTimeout(context.Background(), warmupCloseTimeout)
	defer cancel()
	closeErr := proc.CloseSession(closeCtx, sessionID)
	if closeErr != nil {
		g.log.Warn("closing warmup session failed", "account", account.Name(), "session", sessionID, "err", closeErr)
		if proc.Alive() {
			return
		}
	}
	if err := agy.DeleteConversation(account.GeminiDir(), sessionID); err != nil {
		g.log.Warn("deleting warmup conversation failed", "account", account.Name(), "session", sessionID, "err", err)
	}
}

func (g *Gateway) recordWarmup(rec Record, firstOutput time.Time, usage *conv.Usage, finish conv.FinishReason, err *conv.Error) {
	if g.recorder == nil {
		return
	}
	rec.Duration = time.Since(rec.At)
	if !firstOutput.IsZero() {
		rec.TimeToFirstOutput = firstOutput.Sub(rec.At)
	}
	rec.Usage, rec.Finish, rec.Err = usage, finish, err
	g.recorder.Record(rec)
}

func warmupContextOrUpstreamErr(ctx context.Context, err error) *conv.Error {
	if ctx.Err() != nil {
		return &conv.Error{Kind: conv.ErrCancelled, Message: ctx.Err().Error()}
	}
	return &conv.Error{Kind: conv.ErrUpstream, Message: err.Error()}
}

func warmupModelFailure(account *pool.Account, group string, failure *pool.ModelFailure) *conv.Error {
	switch {
	case failure.Code == http.StatusTooManyRequests:
		wait := time.Until(failure.Reset)
		if wait <= 0 {
			return &conv.Error{Kind: conv.ErrOverloaded, Message: failure.Message}
		}
		account.Cool(group, failure.Reset)
		return &conv.Error{Kind: conv.ErrQuota, Message: failure.Message, RetryAfterSeconds: int(math.Ceil(wait.Seconds()))}
	case failure.Code == http.StatusServiceUnavailable || failure.Code == statusSiteOverloaded:
		return &conv.Error{Kind: conv.ErrOverloaded, Message: failure.Message}
	case failure.Code == http.StatusUnauthorized:
		return &conv.Error{Kind: conv.ErrUnauthorized, Message: failure.Message}
	case failure.Code == http.StatusForbidden:
		return &conv.Error{Kind: conv.ErrForbidden, Message: failure.Message}
	case failure.Code == http.StatusNotFound:
		return &conv.Error{Kind: conv.ErrUnknownModel, Message: failure.Message}
	case failure.Code == http.StatusBadRequest:
		return &conv.Error{Kind: conv.ErrInvalidRequest, Message: failure.Message}
	default:
		return &conv.Error{Kind: conv.ErrUpstream, Message: failure.Message, HTTPStatus: failure.Code}
	}
}
