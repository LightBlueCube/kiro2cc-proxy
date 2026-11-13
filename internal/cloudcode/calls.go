package cloudcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// a session, before agy does: err is nil when the call succeeded.
type ModelCallFunc func(session string, err *CallError)

type sessionKey struct{}

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
	return r.WithContext(context.WithValue(r.Context(), sessionKey{}, session)), nil
}

func callSession(r *http.Request) string {
	session, _ := r.Context().Value(sessionKey{}).(string)
	return session
}

// watchModelCall reports the outcome of a tagged generation call.
func (ap *accountProxy) watchModelCall(resp *http.Response) {
	session := callSession(resp.Request)
	if session == "" {
		return
	}
	if resp.StatusCode/100 == 2 {
		ap.onModelCall(session, nil)
		return
	}
	ap.onModelCall(session, callError(resp))
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
	result.Message = fmt.Sprintf("%s (code %d): %s", body.Error.Status, resp.StatusCode, body.Error.Message)
	for _, d := range body.Error.Details {
		if d.Type != retryInfoType {
			continue
		}
		// An unreadable delay names no wait.
		if delay, err := time.ParseDuration(d.RetryDelay); err == nil {
			result.RetryAfter = delay
		}
	}
	return result
}
