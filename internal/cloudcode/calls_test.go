package cloudcode

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

// quotaRefusal is the 429 Cloud Code sent for Claude's weekly quota (agy
// 1.5.3, 2026-11-13).
const quotaRefusal = `{"error": {"code": 429, "message": "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 139h30m19s.",
  "status": "RESOURCE_EXHAUSTED",
  "details": [{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED", "domain": "cloudcode-pa.googleapis.com",
    "metadata": {"model": "claude-sonnet-4-6", "quotaResetDelay": "139h30m19.791447777s", "quotaResetTimeStamp": "2026-11-19T09:13:59Z", "uiMessage": "true"}},
    {"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "502219.791447777s"}]}}`

// capacityRefusal is the 429 agy retries itself (same day).
const capacityRefusal = `{"error": {"code": 429, "message": "You have exhausted your capacity on this model. Resets in 0s.", "status": "RESOURCE_EXHAUSTED",
  "details": [{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED", "domain": "cloudcode-pa.googleapis.com",
    "metadata": {"model": "claude-sonnet-4-6", "quotaResetDelay": "0s"}}]}}`

type reportedCall struct {
	session string
	err     *CallError
}

func newAccountProxy(onModelCall ModelCallFunc) *accountProxy {
	return &accountProxy{onModelCall: onModelCall, stopped: map[string]time.Time{}}
}

func generationRequest(t *testing.T, requestID string) (*http.Request, []byte) {
	t.Helper()
	body := []byte(`{"project": "aicode-consumers", "requestId": "` + requestID + `", "request": {"contents": []}, "model": "claude-sonnet-4-6"}`)
	r := httptest.NewRequest(http.MethodPost, "/main/v1internal:streamGenerateContent?alt=sse", bytes.NewReader(body))
	if !isGeneration(r) {
		t.Fatal("not taken for a generation call")
	}
	s := &Server{log: slog.New(slog.DiscardHandler)}
	tagged, err := s.withSession("main", r)
	if err != nil {
		t.Fatal(err)
	}
	return tagged, body
}

func TestGenerationCallsAreTaggedWithTheirSession(t *testing.T) {
	r, body := generationRequest(t, "agent/5ba84978-7516-4183-bc0e-5075438a7364/1794581074285/a5cb3a84-1b53-40fd-b04d-e6f1f0682a08/1")
	got := callOf(r)
	if got.session != "5ba84978-7516-4183-bc0e-5075438a7364" || got.model != "claude-sonnet-4-6" || got.capture != nil || !bytes.Equal(got.request, body) {
		t.Fatalf("call = %+v", got)
	}
	if sent, _ := io.ReadAll(r.Body); !bytes.Equal(sent, body) {
		t.Fatalf("body sent upstream = %s", sent)
	}

	checkpoint, _ := generationRequest(t, "checkpoint/19a9b874-d9cd-4c2c-83dc-ce5fa2c6a153")
	if got := callOf(checkpoint); got.session != "" {
		t.Fatalf("checkpoint call tagged with %q", got.session)
	}
}

func TestWatchModelCallReportsGooglesError(t *testing.T) {
	r, reqBody := generationRequest(t, "agent/S1/1/x/1")
	var reports []reportedCall
	ap := newAccountProxy(func(session, model string, err *CallError) bool {
		if model != "claude-sonnet-4-6" {
			t.Errorf("model = %q", model)
		}
		reports = append(reports, reportedCall{session, err})
		return false
	})

	sent := gzipped(t, quotaRefusal)
	quota := response("", http.StatusTooManyRequests, sent, "gzip")
	quota.Request = r
	ap.watchModelCall(quota)
	if left, _ := io.ReadAll(quota.Body); !bytes.Equal(left, sent) {
		t.Fatal("agy did not get the body as it came")
	}

	capacity := response("", http.StatusTooManyRequests, []byte(capacityRefusal), "")
	capacity.Request = r
	ap.watchModelCall(capacity)

	html := response("", http.StatusServiceUnavailable, []byte("<html>busy</html>"), "")
	html.Request = r
	ap.watchModelCall(html)

	ok := response("", http.StatusOK, nil, "")
	ok.Request = r
	ap.watchModelCall(ok)

	noStatus := response("", http.StatusBadRequest, []byte(`{"error": {"code": 400, "message": "Invalid image"}}`), "")
	noStatus.Request = r
	ap.watchModelCall(noStatus)

	hdrQuotaBytes := []byte(`{"error": {"code": 429, "message": "Slow down", "status": "RESOURCE_EXHAUSTED"}}`)
	hdrQuota := response("", http.StatusTooManyRequests, hdrQuotaBytes, "")
	hdrQuota.Header.Set("Retry-After", "12")
	hdrQuota.Request = r
	ap.watchModelCall(hdrQuota)

	want := []reportedCall{
		{"S1", &CallError{Code: 429, RetryAfter: 502219791447777 * time.Nanosecond,
			Message:  "RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 139h30m19s.",
			Request:  reqBody,
			Response: []byte(quotaRefusal)}},
		{"S1", &CallError{Code: 429, Message: "RESOURCE_EXHAUSTED (code 429): You have exhausted your capacity on this model. Resets in 0s.",
			Request: reqBody, Response: []byte(capacityRefusal)}},
		{"S1", &CallError{Code: 503, Message: "HTTP 503 Service Unavailable",
			Request: reqBody, Response: []byte("<html>busy</html>")}},
		{"S1", nil},
		{"S1", &CallError{Code: 400, Message: "HTTP 400: Invalid image",
			Request: reqBody, Response: []byte(`{"error": {"code": 400, "message": "Invalid image"}}`)}},
		{"S1", &CallError{Code: 429, RetryAfter: 12 * time.Second, Message: "RESOURCE_EXHAUSTED (code 429): Slow down",
			Request: reqBody, Response: hdrQuotaBytes}},
	}
	if len(reports) != len(want) {
		t.Fatalf("reports = %+v", reports)
	}
	for i, got := range reports {
		w := want[i]
		if got.session != w.session || (got.err == nil) != (w.err == nil) {
			t.Errorf("report %d = %q %+v, want %+v", i, got.session, got.err, w.err)
			continue
		}
		if got.err != nil && (got.err.Code != w.err.Code || got.err.Message != w.err.Message || got.err.RetryAfter != w.err.RetryAfter ||
			!bytes.Equal(got.err.Request, w.err.Request) || !bytes.Equal(got.err.Response, w.err.Response)) {
			t.Errorf("report %d = %+v, want %+v", i, got.err, w.err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Once the account stops a session, its generation calls are answered
// here with a 403 and never go upstream; other sessions' calls do.
func TestStoppedSessionsCallsDoNotGoUpstream(t *testing.T) {
	sent := 0
	ap := newAccountProxy(func(session, model string, err *CallError) bool { return err != nil && session == "S1" })
	ap.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(&url.URL{Scheme: "https", Host: "upstream.test"}) },
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent++
			status := http.StatusOK
			if callOf(r).session == "S1" {
				status = http.StatusTooManyRequests
			}
			resp := response("", status, []byte(capacityRefusal), "")
			resp.Request = r
			return resp, nil
		}),
		ModifyResponse: func(resp *http.Response) error { ap.watchModelCall(resp); return nil },
	}
	s := &Server{log: slog.New(slog.DiscardHandler), accounts: map[string]*accountProxy{"main": ap}}

	post := func(requestID string) int {
		body := `{"requestId": "` + requestID + `", "model": "m"}`
		r := httptest.NewRequest(http.MethodPost, "/main/v1internal:streamGenerateContent?alt=sse", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	if code := post("agent/S1/1/x/1"); code != http.StatusTooManyRequests {
		t.Fatalf("first call of S1 = %d, want Google's 429", code)
	}
	if code := post("agent/S1/1/x/2"); code != http.StatusForbidden || sent != 1 {
		t.Fatalf("call of the stopped session = %d after %d upstream calls", code, sent)
	}
	if code := post("agent/S2/1/x/1"); code != http.StatusOK || sent != 2 {
		t.Fatalf("call of another session = %d after %d upstream calls", code, sent)
	}
}

func TestUntaggedCallsAreNotReported(t *testing.T) {
	ap := newAccountProxy(func(session, model string, err *CallError) bool {
		t.Fatalf("reported %q %+v", session, err)
		return false
	})
	resp := response("/main/v1internal:streamGenerateContent", http.StatusTooManyRequests, []byte(capacityRefusal), "")
	resp.Request = httptest.NewRequest(http.MethodPost, "/main/v1internal:streamGenerateContent", nil)
	ap.watchModelCall(resp)
}

func TestStoppedSessionIsRefusedUpstream(t *testing.T) {
	stopS1 := false
	ap := newAccountProxy(func(session, model string, err *CallError) bool { return stopS1 && session == "S1" })

	r, _ := generationRequest(t, "agent/S1/1/x/1")
	resp := response("", http.StatusTooManyRequests, []byte(capacityRefusal), "")
	resp.Request = r
	ap.watchModelCall(resp)
	if ap.isStopped("S1") {
		t.Fatal("session stopped though the account did not ask")
	}

	stopS1 = true
	ap.watchModelCall(resp)
	if !ap.isStopped("S1") || ap.isStopped("S2") {
		t.Fatalf("stopped S1 = %v, S2 = %v", ap.isStopped("S1"), ap.isStopped("S2"))
	}

	ap.mu.Lock()
	ap.stopped["S1"] = time.Now().Add(-stoppedFor - time.Second)
	ap.mu.Unlock()
	if ap.isStopped("S1") {
		t.Fatal("session still stopped after its time")
	}
}
