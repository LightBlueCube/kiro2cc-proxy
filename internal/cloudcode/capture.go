package cloudcode

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Capture turns on the recording of generation calls: each one's request
// as agy sent it and the response as Google answered, for debugging. The
// zero value is never used; a nil Capture is the proxy's off state.
type Capture struct {
	// ResponseLimit is the most response bytes kept per call; more are
	// dropped and the call is marked truncated.
	ResponseLimit int64
	// Begin runs when a call that serves a session starts, and returns
	// the tag saved with the call.
	Begin func(account, session string) (tag string)
	// Save receives each call once its response ended.
	Save func(Captured)
}

// Captured is a finished generation call.
type Captured struct {
	At      time.Time
	Account string
	Session string
	// Tag is what Begin returned when the call started.
	Tag string
	// Seq is the call's number within its trajectory.
	Seq    int
	Model  string
	Status int
	// Request is the body agy sent, Response the body Google answered
	// (empty when the call never reached Google).
	Request  []byte
	Response []byte
	// ResponseTruncated is set when Response lost its end to the limit.
	ResponseTruncated bool
}

// SetCapture starts the recording of generation calls with c, or stops it
// when c is nil. Calls already in flight finish under the setting they
// began with.
func (s *Server) SetCapture(c *Capture) { s.capture.Store(c) }

// pendingCapture is a generation call being recorded.
type pendingCapture struct {
	capture *Capture
	call    Captured
}

// beginCapture starts recording a generation call of the session, or
// returns nil when recording is off.
func (s *Server) beginCapture(account, session, requestID, model string, request []byte) *pendingCapture {
	c := s.capture.Load()
	if c == nil {
		return nil
	}
	p := &pendingCapture{capture: c, call: Captured{
		At: time.Now(), Account: account, Session: session, Model: model, Request: request,
		Tag: c.Begin(account, session),
	}}
	slash := strings.LastIndexByte(requestID, '/')
	seq, err := strconv.Atoi(requestID[slash+1:])
	if err != nil {
		s.log.Warn("generation call number unreadable", "account", account, "request_id", requestID, "err", err)
	}
	p.call.Seq = seq
	return p
}

// finish saves the call once, with the response bytes that were kept.
func (p *pendingCapture) finish(status int, response []byte, truncated bool) {
	p.call.Status, p.call.Response, p.call.ResponseTruncated = status, response, truncated
	p.capture.Save(p.call)
}

// recordingBody passes a response body through to agy and keeps its first
// limit bytes; it saves the call when the body ends or closes.
type recordingBody struct {
	io.ReadCloser
	pending   *pendingCapture
	status    int
	kept      bytes.Buffer
	truncated bool
	once      sync.Once
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	room := b.pending.capture.ResponseLimit - int64(b.kept.Len())
	switch {
	case int64(n) <= room:
		b.kept.Write(p[:n])
	case room > 0:
		b.kept.Write(p[:room])
		b.truncated = true
	default:
		b.truncated = b.truncated || n > 0
	}
	if err != nil {
		b.save()
	}
	return n, err
}

func (b *recordingBody) Close() error {
	b.save()
	return b.ReadCloser.Close()
}

func (b *recordingBody) save() {
	b.once.Do(func() { b.pending.finish(b.status, b.kept.Bytes(), b.truncated) })
}
