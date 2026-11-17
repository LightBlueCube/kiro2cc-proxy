package gateway

import (
	"context"
	"fmt"
	"time"

	"agyrelay/internal/conv"
	"agyrelay/internal/pool"
	"agyrelay/internal/sysprompt"
)

// Risk control. Google refuses a request it dislikes, usually because of
// its system prompt, with a 429 that names no wait while the account's
// other requests go through (pool.Account.RecordModelCall tells these
// apart). Such a request is not retried: another account would only be
// refused too, and each try spreads the refusal. Its system prompt is
// flagged for flagDuration instead, and requests carrying it are refused
// here until the flag ends or the admin cancels it; a prompt the admin
// pinned stays refused after the flag ends. The control layer turns the
// whole of this off.

// flagDuration is how long a refused system prompt stays flagged.
const flagDuration = 24 * time.Hour

// flagTimeout bounds saving a flag.
const flagTimeout = 5 * time.Second

// PromptFlags remembers the system prompts Google's risk control refused.
type PromptFlags interface {
	// Flagged is when the flag on the system prompt with this fingerprint
	// (sysprompt.Fingerprint) ends, the zero time when the prompt is not
	// flagged, and whether the admin pinned it: a pinned prompt is
	// refused after its flag ends too. It reports nothing while risk
	// control is off.
	Flagged(fingerprint string) (until time.Time, pinned bool)
	// Flag flags a system prompt, replacing an earlier flag of it.
	Flag(ctx context.Context, prompt FlaggedPrompt) error
}

// FlaggedPrompt is a system prompt that Google's risk control refused.
type FlaggedPrompt struct {
	Fingerprint string
	// Text is the system prompt as the client sent it.
	Text string
	// At is when it was refused; Until is when the flag ends.
	At, Until time.Time
	// KeyHash and KeyName name the API key whose request was refused;
	// Account and Model, where.
	KeyHash, KeyName string
	Account, Model   string
}

// StopRiskySession ends the running turn of a session whose request
// Google's risk control refused: the turn's request fails with
// ErrRiskControl and its system prompt is flagged.
func (g *Gateway) StopRiskySession(account *pool.Account, sessionID string) {
	g.mu.Lock()
	sess := g.sessions[sessionRef(account.Name(), sessionID)]
	if sess == nil || sess.turn == nil || sess.turn.turn == nil {
		g.mu.Unlock()
		g.log.Warn("risk control on a session with no running turn", "account", account.Name(), "session", sessionID)
		return
	}
	sess.turn.risky = true
	turn := sess.turn.turn
	g.mu.Unlock()
	turn.Cancel()
}

// riskControl fails the request of a turn that was stopped for risk
// control, and flags its system prompt when it has one. A request without
// one is refused for something else, which a flag could not name.
func (g *Gateway) riskControl(at *activeTurn, r *run) *attemptFailure {
	account := at.account.Name()
	failure := &attemptFailure{account: account}
	if r.t.system == "" || g.flags == nil {
		g.log.Warn("request refused by risk control", "account", account, "key", r.keyName)
		failure.err = &conv.Error{Kind: conv.ErrRiskControl, Message: "request rejected: Google's risk control refused this request"}
		return failure
	}

	now := time.Now()
	prompt := FlaggedPrompt{
		Fingerprint: sysprompt.Fingerprint(r.t.system), Text: r.t.system,
		At: now, Until: now.Add(flagDuration),
		KeyHash: r.user, KeyName: r.keyName, Account: account, Model: r.model.id,
	}
	ctx, cancel := context.WithTimeout(context.Background(), flagTimeout)
	defer cancel()
	if err := g.flags.Flag(ctx, prompt); err != nil {
		g.log.Error("flagged system prompt not saved", "fingerprint", prompt.Fingerprint, "err", err)
	} else {
		g.log.Warn("system prompt flagged by risk control", "account", account, "key", r.keyName, "fingerprint", prompt.Fingerprint, "until", prompt.Until)
	}
	failure.err = &conv.Error{Kind: conv.ErrRiskControl, Message: flaggedMessage(prompt.Until)}
	return failure
}

// checkFlaggedPrompt refuses a request whose system prompt is flagged.
func (g *Gateway) checkFlaggedPrompt(t *transcript) *conv.Error {
	if g.flags == nil || t.system == "" {
		return nil
	}
	until, pinned := g.flags.Flagged(sysprompt.Fingerprint(t.system))
	if pinned {
		return &conv.Error{Kind: conv.ErrRiskControl, Message: pinnedMessage}
	}
	if !time.Now().Before(until) {
		return nil
	}
	return &conv.Error{Kind: conv.ErrRiskControl, Message: flaggedMessage(until)}
}

const pinnedMessage = "request rejected: Google's risk control refused this system prompt, and the admin keeps it refused. replace it with your own"

func flaggedMessage(until time.Time) string {
	return fmt.Sprintf("request rejected: Google's risk control refused this system prompt, and it stays refused until %s. replace it with your own",
		until.UTC().Format(time.RFC3339))
}
