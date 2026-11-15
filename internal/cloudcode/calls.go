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
}

// ModelCallFunc learns the outcome of each generation call agy makes for
// a session, before agy does: err is nil when the call succeeded, and
// model is the upstream model the call named. It returns true to stop the
// session: the proxy then refuses the session's later generation calls
// instead of sending them upstream.
type ModelCallFunc func(session, model string, err *CallError) (stop bool)

// stoppedFor bounds how long a stopped session's calls are refused; the
// caller ends the session's turn long before.
const stoppedFor = 10 * time.Minute

type callKey struct{}

// call is the generation call a request carries: the session it serves
// and the model it names.
type call struct {
	session, model string
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
	return r.WithContext(context.WithValue(r.Context(), callKey{}, call{session: session, model: body.Model})), nil
}

func callOf(r *http.Request) call {
	c, _ := r.Context().Value(callKey{}).(call)
	return c
}

// watchModelCall reports the outcome of a tagged generation call.
func (ap *accountProxy) watchModelCall(resp *http.Response) {
	c := callOf(resp.Request)
	if c.session == "" {
		return
	}
	if resp.StatusCode/100 == 2 {
		ap.report(c, nil)
		return
	}
	ap.report(c, callError(resp))
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
	if body.Error.Status != "" {
		result.Message = fmt.Sprintf("%s (code %d): %s", body.Error.Status, resp.StatusCode, body.Error.Message)
	} else {
		result.Message = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body.Error.Message)
	}
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
