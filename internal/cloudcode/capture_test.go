package cloudcode

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const sseReply = "data: {\"response\": {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"Hel\"}]}}]}}\n\n" +
	"data: {\"response\": {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"lo\"}]}, \"finishReason\": \"STOP\"}]}}\n\n"

// captureServer serves one account whose upstream answers every call with
// reply and records the Accept-Encoding it was asked for.
func captureServer(reply string, status int, encodings *[]string) *Server {
	ap := newAccountProxy(func(session, model string, err *CallError) bool { return false })
	ap.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "https", Host: "upstream.test"})
			if callOf(r.In).capture != nil {
				r.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*encodings = append(*encodings, r.Header.Get("Accept-Encoding"))
			resp := response("", status, []byte(reply), "")
			resp.Request = r
			return resp, nil
		}),
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			ap.watchModelCall(resp)
			recordResponse(resp)
			return nil
		},
	}
	return &Server{log: slog.New(slog.DiscardHandler), accounts: map[string]*accountProxy{"main": ap}}
}

func postGeneration(s *Server, requestID string) *httptest.ResponseRecorder {
	body := `{"requestId": "` + requestID + `", "model": "m", "request": {"contents": []}}`
	r := httptest.NewRequest(http.MethodPost, "/main/v1internal:streamGenerateContent?alt=sse", strings.NewReader(body))
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

type savedCalls struct {
	mu    sync.Mutex
	calls []Captured
}

func (s *savedCalls) save(c Captured) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}

func TestCaptureKeepsARequestAndItsResponse(t *testing.T) {
	var encodings []string
	s := captureServer(sseReply, http.StatusOK, &encodings)
	var saved savedCalls
	s.SetCapture(&Capture{
		ResponseLimit: 1 << 20,
		Begin:         func(account, session string) string { return account + "|" + session },
		Save:          saved.save,
	})

	w := postGeneration(s, "agent/S1/1794581074285/uuid/7")
	if w.Body.String() != sseReply {
		t.Fatalf("agy got %q", w.Body.String())
	}
	if len(saved.calls) != 1 {
		t.Fatalf("saved %d calls", len(saved.calls))
	}
	got := saved.calls[0]
	if got.Tag != "main|S1" || got.Session != "S1" || got.Account != "main" || got.Seq != 7 || got.Model != "m" || got.Status != http.StatusOK {
		t.Fatalf("captured = %+v", got)
	}
	if string(got.Response) != sseReply || got.ResponseTruncated || !strings.Contains(string(got.Request), `"requestId": "agent/S1/`) {
		t.Fatalf("bodies: request %q, response %q, truncated %v", got.Request, got.Response, got.ResponseTruncated)
	}
	if encodings[0] != "identity" {
		t.Fatalf("upstream was asked for %q", encodings[0])
	}
}

func TestCaptureCutsAResponseAtTheLimit(t *testing.T) {
	var encodings []string
	s := captureServer(sseReply, http.StatusOK, &encodings)
	var saved savedCalls
	s.SetCapture(&Capture{ResponseLimit: 10, Begin: func(string, string) string { return "t" }, Save: saved.save})

	w := postGeneration(s, "agent/S1/1/x/1")
	if w.Body.String() != sseReply {
		t.Fatalf("agy got a cut response: %q", w.Body.String())
	}
	got := saved.calls[0]
	if string(got.Response) != sseReply[:10] || !got.ResponseTruncated {
		t.Fatalf("response %q, truncated %v", got.Response, got.ResponseTruncated)
	}
}

func TestCaptureKeepsRefusals(t *testing.T) {
	var encodings []string
	s := captureServer(capacityRefusal, http.StatusTooManyRequests, &encodings)
	var saved savedCalls
	s.SetCapture(&Capture{ResponseLimit: 1 << 20, Begin: func(string, string) string { return "t" }, Save: saved.save})

	postGeneration(s, "agent/S1/1/x/1")
	if got := saved.calls[0]; got.Status != http.StatusTooManyRequests || string(got.Response) != capacityRefusal {
		t.Fatalf("captured = %d %q", got.Status, got.Response)
	}
}

func TestCaptureSkipsCallsWithoutASessionAndWhileOff(t *testing.T) {
	var encodings []string
	s := captureServer(sseReply, http.StatusOK, &encodings)
	var saved savedCalls
	postGeneration(s, "agent/S1/1/x/1")

	s.SetCapture(&Capture{ResponseLimit: 1 << 20, Begin: func(string, string) string { return "t" }, Save: saved.save})
	postGeneration(s, "checkpoint/19a9b874")
	s.SetCapture(nil)
	postGeneration(s, "agent/S1/1/x/2")

	if len(saved.calls) != 0 {
		t.Fatalf("saved %+v", saved.calls)
	}
	if encodings[0] != "gzip" || encodings[1] != "gzip" {
		t.Fatalf("upstream encodings %v: only captured calls ask for identity", encodings)
	}
}

func TestCaptureSavesOnceWhenTheBodyIsClosedUnread(t *testing.T) {
	var saved savedCalls
	pending := &pendingCapture{capture: &Capture{ResponseLimit: 100, Save: saved.save}}
	body := &recordingBody{ReadCloser: io.NopCloser(strings.NewReader("abc")), pending: pending, status: 200}
	if _, err := body.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	body.Close()
	body.Close()
	if len(saved.calls) != 1 || string(saved.calls[0].Response) != "ab" {
		t.Fatalf("saved %+v", saved.calls)
	}
}
