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
	// ModeFresh replayed the transcript into a new session.
	ModeFresh Mode = "fresh"
)

// Record describes one served request, reported after its last event.
type Record struct {
	At     time.Time
	Key    string
	API    string
	Stream bool
	// Model is the public model id once resolved, else as requested.
	Model      string
	Slug       string
	QuotaGroup string
	// Account and Mode are empty when no attempt started.
	Account string
	Mode    Mode

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
