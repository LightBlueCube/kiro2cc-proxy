package cloudcode

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	if got := callSession(r); got != "5ba84978-7516-4183-bc0e-5075438a7364" {
		t.Fatalf("session = %q", got)
	}
	if sent, _ := io.ReadAll(r.Body); !bytes.Equal(sent, body) {
		t.Fatalf("body sent upstream = %s", sent)
	}

	checkpoint, _ := generationRequest(t, "checkpoint/19a9b874-d9cd-4c2c-83dc-ce5fa2c6a153")
	if got := callSession(checkpoint); got != "" {
		t.Fatalf("checkpoint call tagged with %q", got)
	}
}

func TestWatchModelCallReportsGooglesError(t *testing.T) {
	r, _ := generationRequest(t, "agent/S1/1/x/1")
	var reports []reportedCall
	ap := &accountProxy{onModelCall: func(session string, err *CallError) { reports = append(reports, reportedCall{session, err}) }}

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

	want := []reportedCall{
		{"S1", &CallError{Code: 429, RetryAfter: 502219791447777 * time.Nanosecond,
			Message: "RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 139h30m19s."}},
		{"S1", &CallError{Code: 429, Message: "RESOURCE_EXHAUSTED (code 429): You have exhausted your capacity on this model. Resets in 0s."}},
		{"S1", &CallError{Code: 503, Message: "HTTP 503 Service Unavailable"}},
		{"S1", nil},
	}
	if len(reports) != len(want) {
		t.Fatalf("reports = %+v", reports)
	}
	for i, got := range reports {
		if got.session != want[i].session || (got.err == nil) != (want[i].err == nil) || got.err != nil && *got.err != *want[i].err {
			t.Errorf("report %d = %q %+v, want %+v", i, got.session, got.err, want[i].err)
		}
	}
}

func TestUntaggedCallsAreNotReported(t *testing.T) {
	ap := &accountProxy{onModelCall: func(session string, err *CallError) { t.Fatalf("reported %q %+v", session, err) }}
	resp := response("/main/v1internal:streamGenerateContent", http.StatusTooManyRequests, []byte(capacityRefusal), "")
	resp.Request = httptest.NewRequest(http.MethodPost, "/main/v1internal:streamGenerateContent", nil)
	ap.watchModelCall(resp)
}
