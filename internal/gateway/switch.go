package gateway

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"agyrelay/internal/conv"
)

const (
	// failuresBeforeSwitch is how many requests of a conversation may fail
	// on its account; the request after them is served by another account.
	failuresBeforeSwitch = 3
	// conversationTTL forgets a conversation's failures once its client
	// stops retrying.
	conversationTTL = 10 * time.Minute
	// switchRetryAfterSeconds is the Retry-After of an error whose next
	// request switches accounts at once: the client should not wait long.
	switchRetryAfterSeconds = 10
)

// conversationState is what the gateway remembers of a conversation that
// had failures: the account it is bound to, so a retry stays there, and
// how many requests failed on it.
type conversationState struct {
	account  string
	failures int
	// definite is set by a failure no retry can fix on this account (its
	// quota, its sign-in, a model it lacks).
	definite bool
	// moved is why the conversation replays on account: set when the
	// conversation left its earlier account. Until a reply gives the client
	// an anchor of the new session, the old anchor still names the old one.
	moved ReplayReason
	at    time.Time
}

// switchDue reports whether the conversation's next request leaves the account.
func (s conversationState) switchDue() bool {
	return s.definite || s.failures >= failuresBeforeSwitch
}

func (s conversationState) switchReason() ReplayReason {
	if s.definite {
		return ReplaySwitchDefinite
	}
	return ReplaySwitchFailures
}

// conversations remembers the failed conversations, in memory.
type conversations struct {
	mu    sync.Mutex
	byKey map[string]conversationState
}

func newConversations() *conversations {
	return &conversations{byKey: map[string]conversationState{}}
}

// get returns the state under the first of keys that has one.
func (c *conversations) get(keys []string) (conversationState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		state, ok := c.byKey[key]
		if !ok {
			continue
		}
		if time.Since(state.at) > conversationTTL {
			delete(c.byKey, key)
			continue
		}
		return state, true
	}
	return conversationState{}, false
}

// put stores state under every key, dropping the states that expired.
func (c *conversations) put(keys []string, state conversationState) {
	state.at = time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, old := range c.byKey {
		if time.Since(old.at) > conversationTTL {
			delete(c.byKey, key)
		}
	}
	for _, key := range keys {
		c.byKey[key] = state
	}
}

func (c *conversations) clear(keys []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		delete(c.byKey, key)
	}
}

// conversationKeys name the conversation a request belongs to, for the
// model choice it asks for: a failure is about that model (its account may
// lack it, its capacity may be gone), so a client that goes back to the
// earlier model continues as before. The session its anchor (else its
// longest snapshot prefix) names comes first: it survives edits of the last
// messages. The request's whole content is the fallback: a retry sends it
// again unchanged, even when the failure dropped the snapshot.
func (g *Gateway) conversationKeys(user, choice string, t *transcript) []string {
	prefix := user + "\x00" + choice + "\x00"
	var keys []string
	snap, ok := g.anchoredSnapshot(t)
	if !ok {
		snap, ok = g.snaps.lookup(user, t)
	}
	if ok {
		keys = append(keys, prefix+"session\x00"+sessionRef(snap.Account, snap.SessionID))
	}
	return append(keys, prefix+"content\x00"+t.hashes[len(t.body)])
}

// applyConversation plans r from its conversation's failures: a retry
// stays on the account that failed, and the request after the allowed
// failures leaves it. Leaving is allowed, not forced: with no other
// account that can serve the request, the conversation stays, as it keeps
// its session and reasoning there.
func (g *Gateway) applyConversation(r *run, keys []string) {
	state, ok := g.conversations.get(keys)
	if !ok {
		return
	}
	if state.switchDue() && g.hasOtherAccount(r, state.account) {
		r.exclude[state.account] = true
		r.reuse, r.moving = false, true
		r.moveReason = state.switchReason()
		return
	}
	r.pin = state.account
	if state.moved != "" {
		r.reuse = false
		r.moveReason = state.moved
	}
}

// failConversation counts a failed attempt against its account and turns it
// into the client's error, which tells how many retries are left before
// the conversation switches accounts. Without another account to switch
// to, the failure passes as it is.
func (g *Gateway) failConversation(r *run, keys []string, failure *attemptFailure) *conv.Error {
	r.exclude[failure.account] = true
	if !failure.retry || failure.account == "" || !g.hasOtherAccount(r, failure.account) {
		return g.finalizeFailure(r, failure)
	}
	state, ok := g.conversations.get(keys)
	if !ok || state.account != failure.account {
		state = conversationState{account: failure.account}
	}
	state.failures++
	state.definite = state.definite || failure.definite
	g.conversations.put(keys, state)
	return switchNotice(failure.err, state)
}

const (
	switchWarning = "The next request switches to another account and replays the conversation."
)

// switchNotice returns err with the retries left before the switch. An
// error that switches at once must be retryable by clients: 429 (quota) or
// 503, with a short Retry-After.
func switchNotice(err *conv.Error, state conversationState) *conv.Error {
	out := *err
	switch {
	case state.definite:
		out.Message += "\n\n[agyrelay] This account cannot serve the conversation. " + switchWarning
		out.RetryAfterSeconds = switchRetryAfterSeconds
		out.HTTPStatus = 0
		if out.Kind != conv.ErrQuota {
			out.Kind = conv.ErrOverloaded
		}
	case state.failures >= failuresBeforeSwitch:
		out.Message += fmt.Sprintf("\n\n[agyrelay] The account failed (%d of %d). %s",
			state.failures, failuresBeforeSwitch, switchWarning)
	default:
		out.Message += fmt.Sprintf("\n\n[agyrelay] The account failed (%d of %d). Retries left before switching accounts: %d.",
			state.failures, failuresBeforeSwitch, failuresBeforeSwitch-state.failures)
	}
	return &out
}

// accountError is why an account could not serve a conversation's request.
// reason is safe to show clients; err is for the log.
type accountError struct {
	reason   string
	definite bool
	err      error
}

func (e *accountError) Error() string { return e.reason + ": " + e.err.Error() }
func (e *accountError) Unwrap() error { return e.err }

func newAccountError(reason string, definite bool, cause error) *accountError {
	if cause == nil {
		cause = errors.New(reason)
	}
	return &accountError{reason: reason, definite: definite, err: cause}
}
