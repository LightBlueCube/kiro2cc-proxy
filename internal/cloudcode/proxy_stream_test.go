package cloudcode

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestCancelledRequestBeforeHeadersDoesNotReportBadGateway(t *testing.T) {
	upstreamArrived := make(chan struct{}, 2)
	upstreamDone := make(chan struct{}, 2)
	mode := "hang"
	var modeMu sync.Mutex

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		upstreamArrived <- struct{}{}
		defer func() { upstreamDone <- struct{}{} }()
		modeMu.Lock()
		cur := mode
		modeMu.Unlock()
		if cur == "drop" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()

	srv, err := Start(upstream.URL, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	var mu sync.Mutex
	var reports []reportedCall
	var saved []Captured
	endpoint, err := srv.Register("main", "", nil, nil, nil, func(session, model string, err *CallError) bool {
		mu.Lock()
		reports = append(reports, reportedCall{session, err})
		mu.Unlock()
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.accounts["main"].proxy.Transport = upstream.Client().Transport
	srv.mu.Unlock()
	srv.SetCapture(&Capture{
		ResponseLimit: 1 << 20,
		Begin:         func(account, session string) string { return account + "|" + session },
		Save: func(c Captured) {
			mu.Lock()
			saved = append(saved, c)
			mu.Unlock()
		},
	})

	body := `{"requestId": "agent/S1/1/x/1", "model": "m", "request": {"contents": []}}`

	// 1. Client cancels before upstream writes response headers: must not
	// report a 502 model-call failure or save a bogus 502 capture.
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	reqDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		reqDone <- err
	}()

	select {
	case <-upstreamArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received request")
	}
	cancel()

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream context was not cancelled")
	}
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client request did not finish")
	}

	// Give ErrorHandler a moment to run if it were mistakenly triggered.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	gotReports, gotSaved := len(reports), len(saved)
	mu.Unlock()
	if gotReports != 0 || gotSaved != 0 {
		t.Fatalf("cancelled pre-header call recorded reports=%d saved=%d, want 0/0", gotReports, gotSaved)
	}

	// 2. Contrast: genuine upstream connection drop while client is waiting
	// still reports 502 and saves a 502 capture.
	modeMu.Lock()
	mode = "drop"
	modeMu.Unlock()
	resp, err := http.Post(endpoint+"/v1internal:streamGenerateContent?alt=sse", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	<-upstreamArrived
	<-upstreamDone
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 || reports[0].err == nil || reports[0].err.Code != http.StatusBadGateway {
		t.Fatalf("reports after upstream drop = %+v, want one 502", reports)
	}
	if len(saved) != 1 || saved[0].Status != http.StatusBadGateway {
		t.Fatalf("saved captures after upstream drop = %+v, want one 502", saved)
	}
}
