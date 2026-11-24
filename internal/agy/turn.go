package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultIdleTimeout = 5 * time.Minute
	cancelGrace        = 10 * time.Second
	workerKillGrace    = 2 * time.Second
)

// EventKind discriminates turn events.
type EventKind int

const (
	EventText EventKind = iota + 1
	EventThought
	// EventNativeTool reports one of agy's own tool calls (not the bridge's).
	EventNativeTool
	EventContextUsage
	// EventExternal carries a caller payload queued with Inject, in order
	// with agy's own frames.
	EventExternal
)

type Event struct {
	Kind EventKind
	Text string
	// Native tool name and whether it failed (EventNativeTool).
	Tool   string
	Failed bool
	// Context occupancy (EventContextUsage).
	Used, Size int64
	// Payload of EventExternal.
	Payload any
}

// StopReason values of session/prompt.
const (
	StopEndTurn   = "end_turn"
	StopCancelled = "cancelled"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
)

// Outcome is how a turn ended.
type Outcome struct {
	StopReason string
	Usage      *Usage
	// Err is set when the turn failed (process exit, stall, RPC error).
	Err error
}

// TurnSpec describes one session/prompt.
type TurnSpec struct {
	SessionID string
	// Model is the full ACP slug, effort tier included.
	Model  string
	Prompt []ContentBlock
	// IdleTimeout stops a turn without frames; 0 means the default.
	IdleTimeout time.Duration
}

type promptCall struct {
	done   chan struct{}
	result promptResult
	err    error
}

type steerRequest struct {
	prompt []ContentBlock
	ack    chan error
}

// Turn is one running session/prompt. Events are buffered without bound:
// the consumer may be away while the turn is parked on a client tool.
type Turn struct {
	proc            *Proc
	sessionID       string
	idleAfter       time.Duration
	cancelAfter     time.Duration
	workerKillAfter time.Duration
	steerCh         chan steerRequest
	setupCtx        context.Context
	cancelSetup     context.CancelFunc

	mu          sync.Mutex
	events      []Event
	wake        chan struct{}
	closed      bool
	outcome     Outcome
	parks       int
	natives     map[string]string // toolCallId → name, running native calls
	idleTimer   *time.Timer
	cancelled   bool
	sending     bool // a session/prompt frame is currently being written
	prompted    bool // at least one session/prompt frame was written to agy
	cancelGrace bool // watchCancelGrace has been started
	echoSeen    bool
	held        []mappedUpdate
	text        sanitizer

	done chan struct{}
}

// RunTurn sets the model and starts the prompt. The turn runs until agy
// ends it, Cancel is called, the process exits, or it stalls.
func (p *Proc) RunTurn(spec TurnSpec) *Turn {
	return p.runTurn(spec, cancelGrace, workerKillGrace)
}

func (p *Proc) runTurn(spec TurnSpec, cancelAfter, workerKillAfter time.Duration) *Turn {
	idle := spec.IdleTimeout
	if idle == 0 {
		idle = defaultIdleTimeout
	}
	setupCtx, cancelSetup := context.WithCancel(context.Background())
	t := &Turn{
		proc:            p,
		sessionID:       spec.SessionID,
		idleAfter:       idle,
		cancelAfter:     cancelAfter,
		workerKillAfter: workerKillAfter,
		steerCh:         make(chan steerRequest),
		setupCtx:        setupCtx,
		cancelSetup:     cancelSetup,
		wake:            make(chan struct{}, 1),
		natives:         map[string]string{},
		done:            make(chan struct{}),
	}
	go t.run(spec)
	return t
}

func (t *Turn) run(spec TurnSpec) {
	defer t.cancelSetup()
	promptCtx, stopPrompt := context.WithCancel(context.Background())
	defer stopPrompt()
	go func() {
		select {
		case <-t.proc.Done():
			t.settle(Outcome{Err: fmt.Errorf("agy exited mid-turn (%v)", t.proc.Err())})
		case <-t.done:
		}
	}()

	if err := t.proc.SetConfigOption(t.setupCtx, spec.SessionID, "model", spec.Model); err != nil {
		t.settle(Outcome{Err: fmt.Errorf("select model %s: %w", spec.Model, err)})
		return
	}
	if t.isCancelled() {
		t.settle(Outcome{StopReason: StopCancelled})
		return
	}
	// agy 1.5 offers default, accept-edits, and plan. In default agy asks
	// before native commands, and the permission handler denies them.
	if err := t.proc.SetConfigOption(t.setupCtx, spec.SessionID, "mode", "default"); err != nil {
		if t.isCancelled() {
			t.settle(Outcome{StopReason: StopCancelled})
			return
		}
		t.proc.log.Warn("set mode failed", "session", spec.SessionID, "err", err)
	}

	unsubscribe := t.proc.subscribe(spec.SessionID, t.onUpdate)
	defer unsubscribe()
	t.mu.Lock()
	if t.cancelled || t.closed {
		t.mu.Unlock()
		t.settle(Outcome{StopReason: StopCancelled})
		return
	}
	t.armIdleLocked()
	t.mu.Unlock()

	cur := t.startPrompt(promptCtx, spec.Prompt)
	if cur == nil {
		t.settle(Outcome{StopReason: StopCancelled})
		return
	}
	var total *Usage

	for {
		select {
		case <-cur.done:
			t.finishCur(cur, total)
			return
		case req := <-t.steerCh:
			next, ok := t.handover(promptCtx, cur, &total, req)
			if !ok {
				return
			}
			cur = next
		}
	}
}

// startPrompt writes session/prompt synchronously before waiting in a
// goroutine, so Cancel never writes session/cancel ahead of it. Returns nil
// when the turn is already cancelled or closed.
func (t *Turn) startPrompt(ctx context.Context, blocks []ContentBlock) *promptCall {
	t.mu.Lock()
	if t.cancelled || t.closed {
		t.mu.Unlock()
		return nil
	}
	t.sending = true
	t.mu.Unlock()

	waitFn, err := t.proc.sendPrompt(ctx, t.sessionID, blocks)

	t.mu.Lock()
	t.sending = false
	if err == nil {
		t.prompted = true
	}
	needCancel := t.prompted && t.cancelled && !t.closed
	armGrace := false
	if needCancel && !t.cancelGrace {
		t.cancelGrace = true
		armGrace = true
	}
	t.mu.Unlock()

	call := &promptCall{done: make(chan struct{})}
	if err != nil {
		call.err = err
		close(call.done)
	} else {
		go func() {
			call.result, call.err = waitFn()
			close(call.done)
		}()
	}

	if needCancel {
		t.proc.cancel(t.sessionID)
		if armGrace {
			go t.watchCancelGrace()
		}
	}
	return call
}

func (t *Turn) finishCur(cur *promptCall, total *Usage) {
	if cur.err != nil {
		t.settlePromptErr(cur.err)
		return
	}
	total = addPromptUsage(total, cur.result.Usage)
	t.finishPrompt()
	t.settle(Outcome{StopReason: cur.result.StopReason, Usage: total})
}

// handover sends a mid-turn session/prompt while cur is parked on an MCP tool
// call. agy queues the user message in its language server before ending cur
// with end_turn, so cur's return proves the message is queued.
func (t *Turn) handover(ctx context.Context, cur *promptCall, total **Usage, req steerRequest) (*promptCall, bool) {
	next := t.startPrompt(ctx, req.prompt)
	if next == nil {
		req.ack <- errors.New("antigravity turn cancelled")
		return cur, true
	}

	select {
	case <-cur.done:
		if cur.err != nil || cur.result.StopReason == StopCancelled {
			err := cur.err
			if err == nil {
				err = errors.New("antigravity turn cancelled")
			}
			req.ack <- err
			t.cancelInFlight()
			select {
			case <-next.done:
			case <-t.done:
			}
			if cur.err != nil {
				t.settlePromptErr(cur.err)
			} else {
				t.settle(Outcome{StopReason: StopCancelled})
			}
			return nil, false
		}
		*total = addPromptUsage(*total, cur.result.Usage)
		if t.isCancelled() {
			// A cancel written before agy handed off to next may not have
			// reached next; resend it and keep waiting for next to settle.
			t.cancelInFlight()
			req.ack <- errors.New("antigravity turn cancelled")
			return next, true
		}
		req.ack <- nil
		return next, true

	case <-next.done:
		err := next.err
		if err == nil {
			err = fmt.Errorf("steer session/prompt ended early with %s", next.result.StopReason)
		}
		req.ack <- err
		t.proc.cancel(t.sessionID)
		t.settlePromptErr(err)
		return nil, false
	}
}

func (t *Turn) settlePromptErr(err error) {
	if t.isCancelled() {
		t.settle(Outcome{StopReason: StopCancelled})
		return
	}
	t.settle(Outcome{Err: err})
}

func addPromptUsage(total, part *Usage) *Usage {
	if part == nil {
		return total
	}
	var out Usage
	if total != nil {
		out = *total
	}
	out.InputTokens += part.InputTokens
	out.OutputTokens += part.OutputTokens
	out.ThoughtTokens += part.ThoughtTokens
	out.CachedReadTokens += part.CachedReadTokens
	out.CachedWriteTokens += part.CachedWriteTokens
	out.TotalTokens += part.TotalTokens
	return &out
}

// Next returns the next event, or false once the turn settled and all
// events were consumed (or ctx ended).
func (t *Turn) Next(ctx context.Context) (Event, bool) {
	for {
		if ctx.Err() != nil {
			return Event{}, false
		}
		t.mu.Lock()
		if len(t.events) > 0 {
			ev := t.events[0]
			t.events = t.events[1:]
			t.mu.Unlock()
			return ev, true
		}
		closed := t.closed
		t.mu.Unlock()
		if closed {
			return Event{}, false
		}
		select {
		case <-t.wake:
		case <-ctx.Done():
			return Event{}, false
		}
	}
}

// Done is closed when the turn settled.
func (t *Turn) Done() <-chan struct{} { return t.done }

// Outcome is valid once Done is closed.
func (t *Turn) Outcome() Outcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.outcome
}

// Steer queues a user message into the running session while an MCP tool call
// is parked, and returns once agy accepted it and handed the turn to the new
// session/prompt.
func (t *Turn) Steer(ctx context.Context, prompt []ContentBlock) error {
	req := steerRequest{prompt: prompt, ack: make(chan error, 1)}

	select {
	case t.steerCh <- req:
	case <-t.done:
		return errors.New("antigravity turn ended")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-req.ack:
		return err
	case <-t.done:
		return errors.New("antigravity turn ended")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Inject queues payload as an EventExternal behind the frames received so
// far. Frames held for a prompt echo or a possible system-message block are
// released first, so the payload follows all text agy sent before it. No-op
// once settled.
func (t *Turn) Inject(payload any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.releaseHeldLocked()
	if text := t.text.flush(); text != "" {
		t.pushLocked(Event{Kind: EventText, Text: text})
	}
	t.pushLocked(Event{Kind: EventExternal, Payload: payload})
}

// Park pauses the inactivity guard while a client tool call is outstanding.
func (t *Turn) Park() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.parks++
	t.stopIdleLocked()
}

// Unpark resumes the guard once the last outstanding call returned.
func (t *Turn) Unpark() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.parks > 0 {
		t.parks--
	}
	t.armIdleLocked()
}

// Cancel ends the prompt through ACP. When agy does not settle it within
// the grace period, the worker (or, if needed, the process) is torn down.
func (t *Turn) Cancel() {
	t.mu.Lock()
	if t.closed || t.cancelled {
		t.mu.Unlock()
		return
	}
	t.cancelled = true
	// If startPrompt is writing a session/prompt frame right now, it sends
	// session/cancel immediately after the write so agy sees them in order.
	sendCancel := t.prompted && !t.sending
	armGrace := false
	if sendCancel && !t.cancelGrace {
		t.cancelGrace = true
		armGrace = true
	}
	t.mu.Unlock()

	t.cancelSetup()
	if sendCancel {
		t.proc.cancel(t.sessionID)
		if armGrace {
			go t.watchCancelGrace()
		}
	}
}

func (t *Turn) cancelInFlight() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	armGrace := false
	if !t.cancelGrace {
		t.cancelGrace = true
		armGrace = true
	}
	t.mu.Unlock()

	t.proc.cancel(t.sessionID)
	if armGrace {
		go t.watchCancelGrace()
	}
}

func (t *Turn) watchCancelGrace() {
	timer := time.NewTimer(t.cancelAfter)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
		t.forceStop("agy ignored cancel", Outcome{StopReason: StopCancelled})
	}
}

func (t *Turn) forceStop(reason string, fallback Outcome) {
	select {
	case <-t.done:
		return
	default:
	}
	if t.proc.killWorker(t.sessionID) {
		t.proc.log.Warn(reason+"; killing worker", "session", t.sessionID)
		timer := time.NewTimer(t.workerKillAfter)
		defer timer.Stop()
		select {
		case <-t.done:
			return
		case <-timer.C:
		}
	}
	t.proc.log.Warn(reason+"; killing process", "session", t.sessionID)
	t.proc.Kill()
	t.settle(fallback)
}

func (t *Turn) isCancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancelled
}

func (t *Turn) onUpdate(raw json.RawMessage) {
	update := mapUpdate(raw, BridgeServer)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.idleTimer != nil {
		t.idleTimer.Reset(t.idleAfter)
	}
	// agy echoes the prompt first. Content before the echo belongs to
	// background work that finished between prompts and is dropped.
	if !t.echoSeen {
		if update.kind == updateUserEcho {
			t.echoSeen = true
			t.held = nil
			return
		}
		if isContentUpdate(update.kind) {
			t.held = append(t.held, update)
			return
		}
	}
	t.applyLocked(update)
}

func (t *Turn) applyLocked(update mappedUpdate) {
	switch update.kind {
	case updateText:
		if text := t.text.push(update.text); text != "" {
			t.pushLocked(Event{Kind: EventText, Text: text})
		}
	case updateThought:
		if update.text != "" {
			t.pushLocked(Event{Kind: EventThought, Text: update.text})
		}
	case updateToolStart:
		if update.mcpServer == BridgeServer {
			return // the bridge reports client tool calls itself
		}
		t.natives[update.toolCallID] = update.toolName
		t.stopIdleLocked() // a long native call produces no frames
	case updateToolDone, updateToolFailed:
		name, ok := t.natives[update.toolCallID]
		if !ok {
			return
		}
		delete(t.natives, update.toolCallID)
		t.pushLocked(Event{Kind: EventNativeTool, Tool: name, Failed: update.kind == updateToolFailed})
		t.armIdleLocked()
	case updateContextUsage:
		t.pushLocked(Event{Kind: EventContextUsage, Used: update.used, Size: update.size})
	}
}

func (t *Turn) releaseHeldLocked() {
	if t.echoSeen {
		return
	}
	held := t.held
	t.held = nil
	t.echoSeen = true
	for _, update := range held {
		t.applyLocked(update)
	}
}

// finishPrompt applies frames held for an echo that never came.
func (t *Turn) finishPrompt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseHeldLocked()
	if text := t.text.flush(); text != "" {
		t.pushLocked(Event{Kind: EventText, Text: text})
	}
}

func (t *Turn) pushLocked(ev Event) {
	t.events = append(t.events, ev)
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *Turn) armIdleLocked() {
	t.stopIdleLocked()
	if t.closed || t.parks > 0 || len(t.natives) > 0 {
		return
	}
	t.idleTimer = time.AfterFunc(t.idleAfter, func() {
		t.forceStop("turn stalled", Outcome{Err: fmt.Errorf("antigravity produced no output for %s", t.idleAfter)})
	})
}

func (t *Turn) stopIdleLocked() {
	if t.idleTimer != nil {
		t.idleTimer.Stop()
		t.idleTimer = nil
	}
}

func (t *Turn) settle(outcome Outcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.cancelled {
		outcome = Outcome{StopReason: StopCancelled}
	}
	t.closed = true
	t.outcome = outcome
	t.stopIdleLocked()
	close(t.done)
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// IsAuthError reports whether err means the account must sign in again.
func IsAuthError(err error) bool { return errors.Is(err, ErrAuthRequired) }
