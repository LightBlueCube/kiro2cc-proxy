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

// Turn is one running session/prompt. Events are buffered without bound:
// the consumer may be away while the turn is parked on a client tool.
type Turn struct {
	proc      *Proc
	sessionID string
	idleAfter time.Duration

	mu        sync.Mutex
	events    []Event
	wake      chan struct{}
	closed    bool
	outcome   Outcome
	parks     int
	natives   map[string]string // toolCallId → name, running native calls
	idleTimer *time.Timer
	cancelled bool
	echoSeen  bool
	held      []mappedUpdate
	text      sanitizer

	done chan struct{}
}

// RunTurn sets the model and starts the prompt. The turn runs until agy
// ends it, Cancel is called, the process exits, or it stalls.
func (p *Proc) RunTurn(spec TurnSpec) *Turn {
	idle := spec.IdleTimeout
	if idle == 0 {
		idle = defaultIdleTimeout
	}
	t := &Turn{
		proc:      p,
		sessionID: spec.SessionID,
		idleAfter: idle,
		wake:      make(chan struct{}, 1),
		natives:   map[string]string{},
		done:      make(chan struct{}),
	}
	p.turnStarted()
	go func() {
		<-t.done
		p.turnEnded()
	}()
	go t.run(spec)
	return t
}

func (t *Turn) run(spec TurnSpec) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		select {
		case <-t.proc.Done():
			t.settle(Outcome{Err: fmt.Errorf("agy exited mid-turn (%v)", t.proc.Err())})
		case <-t.done:
		}
	}()

	if err := t.proc.SetConfigOption(ctx, spec.SessionID, "model", spec.Model); err != nil {
		t.settle(Outcome{Err: fmt.Errorf("select model %s: %w", spec.Model, err)})
		return
	}
	// agy 1.5 offers default, accept-edits, and plan. In default agy asks
	// before native commands, and the permission handler denies them.
	if err := t.proc.SetConfigOption(ctx, spec.SessionID, "mode", "default"); err != nil {
		t.proc.log.Warn("set mode failed", "session", spec.SessionID, "err", err)
	}

	unsubscribe := t.proc.subscribe(spec.SessionID, t.onUpdate)
	defer unsubscribe()
	t.mu.Lock()
	t.armIdleLocked()
	t.mu.Unlock()

	result, err := t.proc.prompt(ctx, spec.SessionID, spec.Prompt)
	if err != nil {
		if t.isCancelled() {
			t.settle(Outcome{StopReason: StopCancelled})
			return
		}
		t.settle(Outcome{Err: err})
		return
	}
	t.finishPrompt()
	t.settle(Outcome{StopReason: result.StopReason, Usage: result.Usage})
}

// Next returns the next event, or false once the turn settled and all
// events were consumed (or ctx ended).
func (t *Turn) Next(ctx context.Context) (Event, bool) {
	for {
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

// Inject queues payload as an EventExternal behind the frames received so
// far. Text held for a possible system-message block is released first, so
// the payload follows all text agy sent before it. No-op once settled.
func (t *Turn) Inject(payload any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
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
// the grace period, the process is torn down.
func (t *Turn) Cancel() {
	t.mu.Lock()
	if t.closed || t.cancelled {
		t.mu.Unlock()
		return
	}
	t.cancelled = true
	t.mu.Unlock()

	t.proc.cancel(t.sessionID)
	go func() {
		select {
		case <-t.done:
		case <-time.After(cancelGrace):
			t.proc.log.Warn("agy ignored cancel; killing process", "session", t.sessionID)
			t.proc.Kill()
			t.settle(Outcome{StopReason: StopCancelled})
		}
	}()
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
		if isRetryNotice(update.text) {
			t.pushLocked(Event{Kind: EventThought, Text: "\n" + retryNotice + " agy retries.\n"})
			return
		}
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

// finishPrompt applies frames held for an echo that never came.
func (t *Turn) finishPrompt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.echoSeen {
		held := t.held
		t.held = nil
		t.echoSeen = true
		for _, update := range held {
			t.applyLocked(update)
		}
	}
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
		t.proc.log.Warn("turn stalled; killing process", "session", t.sessionID)
		t.proc.Kill()
		t.settle(Outcome{Err: fmt.Errorf("antigravity produced no output for %s", t.idleAfter)})
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
	if t.cancelled && outcome.Err == nil {
		outcome.StopReason = StopCancelled
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
