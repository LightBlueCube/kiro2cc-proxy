package cloudcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// agy's generation calls name the ACP session they serve in the body:
// "requestId": "agent/<session id>/<unix ms>/<uuid>/<n>" (verified on
// 1.5.3). Its checkpoint summaries ("checkpoint/<uuid>") serve none.
const (
	generationSuffix   = "/v1internal:streamGenerateContent"
	agentRequestPrefix = "agent/"
	retryInfoType      = "type.googleapis.com/google.rpc.RetryInfo"
)

// CallError is a generation call that failed: Google refused it, or it
// never reached Google.
type CallError struct {
	// Code is the HTTP status agy got.
	Code int
	// Message is the error as agy words it: "RESOURCE_EXHAUSTED (code
	// 429): Individual quota reached. … Resets in 139h30m19s."
	Message string
	// RetryAfter is the wait Google's RetryInfo names; 0 when none.
	RetryAfter time.Duration
	// Request is the body agy sent; Response is the decoded error body
	// Google answered (empty when unreadable or the call never reached Google).
	Request, Response []byte
	// Truncated marks a call that answered but stopped at the output
	// token limit (Code is 200): not a failure, but its reply is cut off.
	Truncated bool
}

// ModelCallFunc learns the outcome of each generation call agy makes for
// a session, before agy does: err is nil when the call succeeded, and
// model is the upstream model the call named. It returns true to stop the
// session: the proxy then refuses the session's later generation calls
// instead of sending them upstream. The call's response stream is judged
// when it ends, so err can describe a call Google answered with 200.
type ModelCallFunc func(session, model string, err *CallError) (stop bool)

// stoppedFor bounds how long a stopped session's calls are refused; the
// caller ends the session's turn long before.
const stoppedFor = 10 * time.Minute

type callKey struct{}

// call is the generation call a request carries: the session it serves
// and the model it names.
type call struct {
	session, model string
	request        []byte
	// capture is set while the call is being recorded.
	capture *pendingCapture
}

func isGeneration(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, generationSuffix)
}

// withSession tags a generation call with the session it serves, leaving
// its body for upstream as it was. Calls that serve none stay untagged.
func (s *Server) withSession(account string, r *http.Request) (*http.Request, error) {
	raw, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))

	var body struct {
		RequestID string `json:"requestId"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		s.log.Warn("generation call not watched: body is not JSON", "account", account, "err", err)
		return r, nil
	}
	rest, ok := strings.CutPrefix(body.RequestID, agentRequestPrefix)
	if !ok {
		return r, nil
	}
	session, _, _ := strings.Cut(rest, "/")
	c := call{session: session, model: body.Model, request: raw, capture: s.beginCapture(account, session, body.RequestID, body.Model, raw)}
	return r.WithContext(context.WithValue(r.Context(), callKey{}, c)), nil
}

func callOf(r *http.Request) call {
	c, _ := r.Context().Value(callKey{}).(call)
	return c
}

// recordResponse saves a call being captured once agy has read its
// response.
func recordResponse(resp *http.Response) {
	c := callOf(resp.Request)
	if c.capture == nil {
		return
	}
	resp.Body = &recordingBody{ReadCloser: resp.Body, pending: c.capture, status: resp.StatusCode}
}

// watchModelCall reports the outcome of a tagged generation call: at
// once for a refusal, when its stream ends for an answer.
func (ap *accountProxy) watchModelCall(resp *http.Response) {
	c := callOf(resp.Request)
	if c.session == "" {
		return
	}
	if resp.StatusCode/100 == 2 {
		ap.judgeStream(c, resp)
		return
	}
	ce := callError(resp)
	ce.Request = c.request
	ap.report(c, ce)
}

// report hands a call's outcome to the account and stops the session when
// it asks.
func (ap *accountProxy) report(c call, err *CallError) {
	if !ap.onModelCall(c.session, c.model, err) {
		return
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	now := time.Now()
	for session, at := range ap.stopped {
		if now.Sub(at) > stoppedFor {
			delete(ap.stopped, session)
		}
	}
	ap.stopped[c.session] = now
}

// isStopped reports whether the session was stopped.
func (ap *accountProxy) isStopped(session string) bool {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	at, ok := ap.stopped[session]
	return ok && time.Since(at) <= stoppedFor
}

// describeError words Google's error as agy does.
func describeError(code int, status, message string) string {
	if status != "" {
		return fmt.Sprintf("%s (code %d): %s", status, code, message)
	}
	return fmt.Sprintf("HTTP %d: %s", code, message)
}

// callError reads Google's error from resp, leaving its body for agy as it
// was:
//
//	{"error": {"code": 429, "message": "…", "status": "RESOURCE_EXHAUSTED",
//	  "details": [{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "502219.79s"}, …]}}
func callError(resp *http.Response) *CallError {
	result := &CallError{Code: resp.StatusCode, Message: fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))}
	raw, ok := readErrorBody(resp)
	if !ok {
		return result
	}
	result.Response = raw
	var body struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Error.Message == "" {
		return result
	}
	result.Message = describeError(resp.StatusCode, body.Error.Status, body.Error.Message)
	for _, d := range body.Error.Details {
		if d.Type != retryInfoType {
			continue
		}
		// An unreadable delay names no wait.
		if delay, err := time.ParseDuration(d.RetryDelay); err == nil {
			result.RetryAfter = delay
		}
	}
	if result.RetryAfter == 0 {
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			result.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return result
}
