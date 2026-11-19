package cloudcode

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

// proxied serves a generation call of session S1 through a real reverse
// proxy to upstream, as Register builds it, and returns what the client
// read and what the account was told.
func proxied(t *testing.T, upstream http.HandlerFunc) (read string, readErr error, reports []reportedCall) {
	t.Helper()
	backend := httptest.NewServer(upstream)
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	ap := newAccountProxy(func(session, model string, err *CallError) bool {
		reports = append(reports, reportedCall{session, err})
		return false
	})
	ap.proxy = &httputil.ReverseProxy{
		Rewrite:        func(r *httputil.ProxyRequest) { r.SetURL(target) },
		FlushInterval:  -1,
		ModifyResponse: func(resp *http.Response) error { ap.watchModelCall(resp); return nil },
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tagged, _ := generationRequest(t, "agent/S1/1/x/1")
		ap.proxy.ServeHTTP(w, tagged)
	}))
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1internal:streamGenerateContent?alt=sse", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	return string(body), readErr, reports
}

func serveSSE(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

func TestProxiedStreamIsJudgedWhenItEnds(t *testing.T) {
	read, err, reports := proxied(t, func(w http.ResponseWriter, r *http.Request) { serveSSE(w, stopStream) })
	if err != nil || read != stopStream {
		t.Fatalf("client read %q, %v", read, err)
	}
	if len(reports) != 1 || reports[0].err != nil {
		t.Fatalf("reports = %+v", reports)
	}

	read, err, reports = proxied(t, func(w http.ResponseWriter, r *http.Request) { serveSSE(w, event(textEvent)) })
	if err != nil || read != event(textEvent) {
		t.Fatalf("client read %q, %v", read, err)
	}
	if len(reports) != 1 || reports[0].err == nil || !strings.Contains(reports[0].err.Message, "without a finish reason") {
		t.Fatalf("reports = %+v", reports)
	}
}

// A stream the upstream cuts after some text is passed on as far as it
// came, and judged as interrupted.
func TestProxiedStreamCutByUpstreamIsInterrupted(t *testing.T) {
	_, _, reports := proxied(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, event(textEvent))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.(*net.TCPConn).SetLinger(0)
		conn.Close()
	})
	if len(reports) != 1 || reports[0].err == nil || reports[0].err.Code != http.StatusBadGateway ||
		!strings.Contains(reports[0].err.Message, "stream interrupted") {
		t.Fatalf("reports = %+v", reports)
	}
}
