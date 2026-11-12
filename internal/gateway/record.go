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
	// ReplayWarmFailed: the session could not be reopened or rewound.
	ReplayWarmFailed ReplayReason = "warm_failed"
	// ReplayRetry: an earlier attempt failed on another account.
	ReplayRetry ReplayReason = "retry"
)

// Record describes one served request, reported after its last event.
type Record struct {
	At      time.Time
	KeyHash string
	KeyName string
	API     string
	Stream  bool
	// Model is the public model id once resolved, else as requested.
	Model string
	Slug  string
	// Effort is the tier sent to agy, "" for a model without tiers.
	Effort     string
	QuotaGroup string
	// Account and Mode are empty when no attempt started.
	Account string
	Mode    Mode
	// ReplayReason is set when Mode is ModeReplay.
	ReplayReason ReplayReason

	Duration time.Duration
	// TimeToFirstOutput is 0 when nothing was output.
	TimeToFirstOutput time.Duration
	// Usage is set on the response that ended the agy turn: it covers the
	// whole turn, including earlier responses ended by tool calls.
	Usage  *conv.Usage
	Finish conv.FinishReason
	Err    *conv.Error
}

// Recorder receives one Record per request.
type Recorder interface {
	Record(Record)
}
