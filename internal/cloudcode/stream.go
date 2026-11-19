package cloudcode

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// A generation call that answers 200 can still fail: its stream breaks,
// carries an error event, ends without a finish reason or holds nothing
// usable, or stops at a finish reason that is no answer. agy reports none
// of this: it retries, then ends its turn with a line like "⚠ Agent
// execution terminated due to error." as the reply. So the proxy reads
// each stream on its way to agy and judges the call when the stream ends,
// before agy sees the end and writes that line. Every call Google
// answered in a recording of 359 (Gemini and Claude) held content and
// ended with a single STOP.
const (
	sseDataPrefix   = "data:"
	finishStop      = "STOP"
	finishMaxTokens = "MAX_TOKENS"
)

// blockedFinishReasons are the finish reasons agy words as "blocked by
// content safety filters"; asking again gets no other answer.
var blockedFinishReasons = map[string]bool{"SAFETY": true, "RECITATION": true, "PROHIBITED_CONTENT": true}

// errClosedEarly is why a stream's body was closed before it ended.
var errClosedEarly = errors.New("stream closed before its end")

// streamEvent is one "data:" event of a generation stream.
type streamEvent struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
	Response struct {
		Candidates []struct {
			Content struct {
				Parts []map[string]json.RawMessage `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	} `json:"response"`
}

// streamJudge reads a generation stream, as it is written to it, and
// judges the call.
type streamJudge struct {
	// partial is the last line, still without its end.
	partial     []byte
	content     bool
	finish      string
	blockReason string
	// streamError is the first error event.
	streamError *CallError
	// unreadable is why the stream could not be read, nil if it could.
	unreadable error
}

// Write takes the next bytes of the stream; it never fails.
func (j *streamJudge) Write(p []byte) (int, error) {
	j.partial = append(j.partial, p...)
	for {
		line, rest, found := bytes.Cut(j.partial, []byte("\n"))
		if !found {
			return len(p), nil
		}
		j.readLine(line)
		j.partial = rest
	}
}

func (j *streamJudge) readLine(line []byte) {
	data, isData := strings.CutPrefix(strings.TrimSpace(string(line)), sseDataPrefix)
	data = strings.TrimSpace(data)
	if !isData || data == "" {
		return
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		j.fail(fmt.Errorf("unreadable stream event: %w", err))
		return
	}
	if ev.Error != nil && j.streamError == nil {
		code := ev.Error.Code
		if code == 0 {
			code = http.StatusBadGateway
		}
		j.streamError = &CallError{Code: code, Message: describeError(code, ev.Error.Status, ev.Error.Message)}
	}
	if reason := ev.Response.PromptFeedback.BlockReason; reason != "" {
		j.blockReason = reason
	}
	for _, candidate := range ev.Response.Candidates {
		if candidate.FinishReason != "" {
			j.finish = candidate.FinishReason
		}
		for _, part := range candidate.Content.Parts {
			j.content = j.content || partHoldsContent(part)
		}
	}
}

func (j *streamJudge) fail(err error) {
	if j.unreadable == nil {
		j.unreadable = err
	}
}

// partHoldsContent reports whether a part carries output: text, or a
// function call, image or other payload. Empty text and thought signatures
// alone are not output.
func partHoldsContent(part map[string]json.RawMessage) bool {
	for key, value := range part {
		switch key {
		case "thought", "thoughtSignature":
		case "text":
			if string(value) != `""` {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// verdict judges the call once its stream ended; nil is a call that
// answered. A call cut off at the output token limit is Truncated.
func (j *streamJudge) verdict() *CallError {
	j.readLine(j.partial)
	j.partial = nil

	switch {
	case j.streamError != nil:
		return j.streamError
	case j.unreadable != nil:
		return badGateway(j.unreadable.Error())
	case j.blockReason != "":
		return blocked("promptFeedback blockReason " + j.blockReason)
	case blockedFinishReasons[j.finish]:
		return blocked("finishReason " + j.finish)
	case j.finish == finishMaxTokens:
		return &CallError{Code: http.StatusOK, Message: "the response stopped at the output token limit", Truncated: true}
	case j.finish == "" && j.content:
		return badGateway("stream ended without a finish reason")
	case j.finish == "":
		return badGateway("empty response")
	case j.finish != finishStop:
		return badGateway("model call ended with finish reason " + j.finish)
	case !j.content:
		return badGateway("empty response")
	}
	return nil
}

func badGateway(message string) *CallError {
	return &CallError{Code: http.StatusBadGateway, Message: message}
}

// blocked is a call Google's content filter stopped. It is a 400, which
// the gateway passes on without a retry.
func blocked(why string) *CallError {
	return &CallError{Code: http.StatusBadRequest, Message: "response blocked by Google's content safety filter (" + why + ")"}
}

// streamTap hands a copy of a response stream, decoded, to a judge.
type streamTap struct {
	judge *streamJudge
	in    io.WriteCloser
	// done is closed once the judge has read everything written to in.
	done <-chan struct{}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// newStreamTap taps a stream in the given Content-Encoding; false for an
// encoding it cannot decode.
func newStreamTap(encoding string) (*streamTap, bool) {
	judge := &streamJudge{}
	done := make(chan struct{})
	switch encoding {
	case "", "identity":
		close(done)
		return &streamTap{judge: judge, in: nopWriteCloser{judge}, done: done}, true
	case "gzip":
		pr, pw := io.Pipe()
		go func() {
			defer close(done)
			judge.readGzip(pr)
		}()
		return &streamTap{judge: judge, in: pw, done: done}, true
	}
	return nil, false
}

// readGzip judges a gzip stream, and keeps taking it after a decoding
// failure so the writer never blocks.
func (j *streamJudge) readGzip(r *io.PipeReader) {
	zr, err := gzip.NewReader(r)
	if err == nil {
		_, err = io.Copy(j, zr)
	}
	if err != nil {
		j.fail(fmt.Errorf("undecodable gzip stream: %w", err))
	}
	// The pipe reader is never closed with an error, so this ends with
	// the writer's close.
	_, _ = io.Copy(io.Discard, r)
}

// finish ends the tap and waits for the judge to read all that was written.
func (t *streamTap) finish() {
	t.in.Close()
	<-t.done
}

// judgingBody passes a response body through to agy and judges the call
// when the body ends, before agy sees the end.
type judgingBody struct {
	io.ReadCloser
	tap *streamTap
	// gone is closed once agy left; a call it left is not judged.
	gone   <-chan struct{}
	report func(*CallError)
	once   sync.Once
}

func (b *judgingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if _, werr := b.tap.in.Write(p[:n]); werr != nil {
		b.tap.judge.fail(werr)
	}
	if err != nil {
		b.end(err)
	}
	return n, err
}

func (b *judgingBody) Close() error {
	b.end(errClosedEarly)
	return b.ReadCloser.Close()
}

// end reports the call once; readErr is how the body ended: io.EOF for a
// stream that ended cleanly.
func (b *judgingBody) end(readErr error) {
	b.once.Do(func() {
		b.tap.finish()
		if verdict, judged := b.outcome(readErr); judged {
			b.report(verdict)
		}
	})
}

// outcome judges the call; false for a call agy left before it ended.
func (b *judgingBody) outcome(readErr error) (*CallError, bool) {
	judge := b.tap.judge
	switch {
	case readErr == io.EOF:
		return judge.verdict(), true
	case errors.Is(readErr, errClosedEarly):
		// Nothing broke the stream: agy stopped reading it. One that
		// already finished is judged by what it held.
		return judge.verdict(), judge.finish != ""
	}
	select {
	case <-b.gone:
		return nil, false
	default:
	}
	return badGateway("stream interrupted: " + readErr.Error()), true
}

// judgeStream reports the outcome of a 2xx generation call once its
// stream ended. A stream in an encoding the proxy cannot decode passes
// unjudged and counts as a success.
func (ap *accountProxy) judgeStream(c call, resp *http.Response) {
	encoding := resp.Header.Get("Content-Encoding")
	tap, decodable := newStreamTap(encoding)
	if !decodable {
		ap.log.Warn("generation call not judged", "session", c.session, "content_encoding", encoding)
		ap.report(c, nil)
		return
	}
	resp.Body = &judgingBody{ReadCloser: resp.Body, tap: tap, gone: resp.Request.Context().Done(), report: func(err *CallError) {
		if err != nil {
			err.Request = c.request
		}
		ap.report(c, err)
	}}
}
