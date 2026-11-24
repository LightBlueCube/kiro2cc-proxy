package gateway

import (
	"time"

	"agyrelay/internal/conv"
)

// Mode is how a request reached its ACP session.
type Mode string

const (
	// ModeContinue answered the tool calls of a parked turn.
	ModeContinue Mode = "continue"
	// ModeWarm prompted a session holding the request's prefix.
	ModeWarm Mode = "warm"
	// ModeRewind rewound a session to the request's prefix first.
	ModeRewind Mode = "rewind"
	// ModeFresh started a new session for a conversation without history.
	ModeFresh Mode = "fresh"
	// ModeReplay replayed the conversation's history into a new session;
	// the Record's ReplayReason says why no session was reused.
	ModeReplay Mode = "replay"
)

// ReplayReason is why a request with history replayed it.
type ReplayReason string

const (
	// ReplayNoSnapshot: no session holds the request's prefix (a new
	// conversation to the relay, an expired snapshot, an edited history).
	ReplayNoSnapshot ReplayReason = "no_snapshot"
	// ReplayToolsChanged: the session's tool set differs from the request's.
	ReplayToolsChanged ReplayReason = "tools_changed"
	// ReplaySwitchFailures: the conversation's account failed its requests
	// too often, so the conversation moved to another account.
	ReplaySwitchFailures ReplayReason = "switch_failures"
	// ReplaySwitchDefinite: the account could not serve the conversation
	// at all (quota, sign-in, model), so it moved to another account.
	ReplaySwitchDefinite ReplayReason = "switch_definite"
)

// AnchorStatus reports whether the request returned a usable anchor
// (encrypted_content / reasoning.encrypted) for its conversation history.
type AnchorStatus string

const (
	// AnchorNone means the request had no prior assistant turn, so no anchor
	// was expected.
	AnchorNone AnchorStatus = ""
	// AnchorOK means a well-formed agr1. anchor matched the transcript prefix
	// through the latest assistant message.
	AnchorOK AnchorStatus = "ok"
	// AnchorStale means a well-formed agr1. anchor matched an earlier prefix,
	// but not the latest assistant message.
	AnchorStale AnchorStatus = "stale"
	// AnchorInvalid means the client sent encrypted_content / reasoning.encrypted
	// data, but none matched the transcript prefix.
	AnchorInvalid AnchorStatus = "invalid"
	// AnchorMissing means the request had prior assistant turns and the client
	// sent no anchor data at all.
	AnchorMissing AnchorStatus = "missing"
)

// Record describes one served request, reported after its last event.
type Record struct {
	At      time.Time
	KeyHash string
	KeyName string
	API     string
	Stream  bool
	Anchor  AnchorStatus
	// Model is the public model id once resolved, else as requested.
	Model string
	Slug  string
	// Effort is the tier sent to agy, "" for a model without tiers.
	Effort string
	// QuotaGroup is the quota group the serving account charged.
	QuotaGroup string
	// Account (the account ID), AccountName, and Mode are empty when no
	// attempt started.
	Account     string
	AccountName string
	// SessionID is the ACP session that served the request, empty when no
	// attempt started.
	SessionID string
	Mode      Mode
	// ReplayReason is set when Mode is ModeReplay.
	ReplayReason ReplayReason

	Duration time.Duration
	// TimeToFirstOutput is 0 when nothing was output.
	TimeToFirstOutput time.Duration
	// Usage is the model calls the response ended with: those since the
	// turn's previous response. A response that ends on tool calls reports
	// its calls when it ends, so a parked turn has none unrecorded. It is
	// nil when the language server could not tell; the turn's last
	// response then covers them.
	Usage  *conv.Usage
	Finish conv.FinishReason
	Err    *conv.Error
	// CallToken names the request; the model calls its turn makes carry
	// it (Gateway.CallTokenOf), so captured upstream calls find their
	// request.
	CallToken string
}

// Recorder receives one Record per request.
type Recorder interface {
	Record(Record)
}
