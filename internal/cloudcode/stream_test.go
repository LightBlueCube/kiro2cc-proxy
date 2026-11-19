package cloudcode

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// event is one stream event as Cloud Code sends it, with its CRLF end.
func event(response string) string {
	return `data: {"response": ` + response + `, "traceId": "t", "metadata": {}}` + "\r\n\r\n"
}

const (
	textEvent     = `{"candidates": [{"content": {"role": "model", "parts": [{"text": "Pong"}]}}]}`
	stopEvent     = `{"candidates": [{"content": {"role": "model", "parts": [{"thoughtSignature": "sig", "text": ""}]}, "finishReason": "STOP"}]}`
	thoughtEvent  = `{"candidates": [{"content": {"role": "model", "parts": [{"thought": true, "text": ""}]}}]}`
	callEvent     = `{"candidates": [{"content": {"role": "model", "parts": [{"functionCall": {"name": "ls", "args": {}}}]}}]}`
	emptyTextStop = `{"candidates": [{"content": {"role": "model", "parts": [{"text": ""}]}, "finishReason": "STOP"}]}`
)

var stopStream = event(thoughtEvent) + event(textEvent) + event(stopEvent)

func finishEvent(reason string) string {
	return event(`{"candidates": [{"content": {"role": "model", "parts": [{"text": "x"}]}, "finishReason": "` + reason + `"}]}`)
}

// servedCall is a call of session S1 whose response carries body.
type servedCall struct {
	t       *testing.T
	reports []reportedCall
	resp    *http.Response
}

func serve(t *testing.T, body io.Reader, encoding string) *servedCall {
	t.Helper()
	s := &servedCall{t: t}
	r, _ := generationRequest(t, "agent/S1/1/x/1")
	ap := newAccountProxy(func(session, model string, err *CallError) bool {
		s.reports = append(s.reports, reportedCall{session, err})
		return false
	})
	s.resp = response("", http.StatusOK, nil, encoding)
	s.resp.Body = io.NopCloser(body)
	s.resp.Request = r
	ap.watchModelCall(s.resp)
	return s
}

// readAll plays agy: it reads the whole body and closes it.
func (s *servedCall) readAll() ([]byte, error) {
	read, err := io.ReadAll(s.resp.Body)
	s.resp.Body.Close()
	return read, err
}

func TestJudgeStreamVerdicts(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		code   int // 0: the call answered
		part   string
	}{
		{"text then STOP", stopStream, 0, ""},
		{"function call then STOP", event(callEvent) + finishEvent("STOP"), 0, ""},
		{"STOP in a bare final event", event(textEvent) + event(emptyTextStop), 0, ""},
		{"unterminated last event", strings.TrimSuffix(stopStream, "\r\n\r\n"), 0, ""},
		{"no finish reason after text", event(textEvent), 502, "without a finish reason"},
		{"empty stream", "", 502, "empty response"},
		{"empty text", event(emptyTextStop), 502, "empty response"},
		{"empty parts", event(`{"candidates": [{"content": {"role": "model", "parts": []}, "finishReason": "STOP"}]}`), 502, "empty response"},
		{"no candidates", event(`{"candidates": []}`), 502, "empty response"},
		{"OTHER", event(`{"candidates": [{"finishReason": "OTHER"}]}`), 502, "OTHER"},
		{"MALFORMED_FUNCTION_CALL", event(`{"candidates": [{"finishReason": "MALFORMED_FUNCTION_CALL"}]}`), 502, "MALFORMED_FUNCTION_CALL"},
		{"SAFETY", event(textEvent) + finishEvent("SAFETY"), 400, "SAFETY"},
		{"RECITATION", finishEvent("RECITATION"), 400, "RECITATION"},
		{"PROHIBITED_CONTENT", finishEvent("PROHIBITED_CONTENT"), 400, "PROHIBITED_CONTENT"},
		{"blocked prompt", event(`{"promptFeedback": {"blockReason": "PROHIBITED_CONTENT"}}`), 400, "blockReason PROHIBITED_CONTENT"},
		{"error event first", `data: {"error": {"code": 503, "message": "overloaded", "status": "UNAVAILABLE"}}` + "\r\n\r\n", 503, "UNAVAILABLE (code 503): overloaded"},
		{"error event after text", event(textEvent) + `data: {"error": {"message": "broke"}}` + "\r\n\r\n", 502, "HTTP 502: broke"},
		{"unreadable event", event(textEvent) + "data: {nope\r\n\r\n", 502, "unreadable stream event"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := serve(t, strings.NewReader(tc.stream), "")
			if len(s.reports) != 0 {
				t.Fatalf("reported before the stream ended: %+v", s.reports)
			}
			read, err := s.readAll()
			if err != nil || string(read) != tc.stream {
				t.Fatalf("agy read %q, %v", read, err)
			}
			if len(s.reports) != 1 || s.reports[0].session != "S1" {
				t.Fatalf("reports = %+v", s.reports)
			}
			got := s.reports[0].err
			if tc.code == 0 {
				if got != nil {
					t.Fatalf("judged a failure: %+v", got)
				}
				return
			}
			if got == nil || got.Code != tc.code || !strings.Contains(got.Message, tc.part) {
				t.Fatalf("verdict = %+v, want %d containing %q", got, tc.code, tc.part)
			}
			if got.Truncated || len(got.Request) == 0 {
				t.Fatalf("verdict = %+v", got)
			}
		})
	}
}

func TestJudgeStreamMarksTheOutputTokenLimit(t *testing.T) {
	s := serve(t, strings.NewReader(event(textEvent)+finishEvent("MAX_TOKENS")), "")
	s.readAll()
	if len(s.reports) != 1 || s.reports[0].err == nil || !s.reports[0].err.Truncated || s.reports[0].err.Code != http.StatusOK {
		t.Fatalf("reports = %+v", s.reports)
	}
}

func TestJudgeStreamAcrossChunkBoundaries(t *testing.T) {
	readers := map[string]func(io.Reader) io.Reader{
		"one byte at a time": iotest.OneByteReader,
		"data and error":     iotest.DataErrReader,
		"half reads":         iotest.HalfReader,
	}
	for name, wrap := range readers {
		s := serve(t, wrap(strings.NewReader(stopStream)), "")
		read, err := s.readAll()
		if err != nil || string(read) != stopStream {
			t.Fatalf("%s: agy read %q, %v", name, read, err)
		}
		if len(s.reports) != 1 || s.reports[0].err != nil {
			t.Fatalf("%s: reports = %+v", name, s.reports)
		}
	}
}

func TestJudgeStreamReadsGzipOnACopy(t *testing.T) {
	sent := gzipped(t, stopStream)
	s := serve(t, bytes.NewReader(sent), "gzip")
	read, err := s.readAll()
	if err != nil || !bytes.Equal(read, sent) {
		t.Fatal("agy did not get the body as it came")
	}
	if len(s.reports) != 1 || s.reports[0].err != nil {
		t.Fatalf("reports = %+v", s.reports)
	}

	s = serve(t, bytes.NewReader(gzipped(t, event(textEvent))), "gzip")
	s.readAll()
	if len(s.reports) != 1 || s.reports[0].err == nil || !strings.Contains(s.reports[0].err.Message, "without a finish reason") {
		t.Fatalf("reports = %+v", s.reports)
	}

	s = serve(t, strings.NewReader("not gzip"), "gzip")
	s.readAll()
	if len(s.reports) != 1 || s.reports[0].err == nil || !strings.Contains(s.reports[0].err.Message, "undecodable gzip") {
		t.Fatalf("reports = %+v", s.reports)
	}
}

func TestJudgeStreamReportsABrokenStream(t *testing.T) {
	cut := io.MultiReader(strings.NewReader(event(textEvent)), iotest.ErrReader(errors.New("connection reset by peer")))
	s := serve(t, cut, "")
	if _, err := s.readAll(); err == nil {
		t.Fatal("agy was not told the stream broke")
	}
	if len(s.reports) != 1 || s.reports[0].err == nil || s.reports[0].err.Code != http.StatusBadGateway ||
		!strings.Contains(s.reports[0].err.Message, "stream interrupted: connection reset by peer") {
		t.Fatalf("reports = %+v", s.reports)
	}
}

func TestJudgeStreamLeavesACallAgyAbandoned(t *testing.T) {
	s := serve(t, strings.NewReader(event(textEvent)), "")
	s.resp.Body.Close()
	if len(s.reports) != 0 {
		t.Fatalf("an abandoned call was judged: %+v", s.reports)
	}

	// One that delivered its finish reason first is judged by its content.
	s = serve(t, strings.NewReader(stopStream), "")
	if _, err := io.ReadFull(s.resp.Body, make([]byte, len(stopStream))); err != nil {
		t.Fatal(err)
	}
	s.resp.Body.Close()
	if len(s.reports) != 1 || s.reports[0].err != nil {
		t.Fatalf("reports = %+v", s.reports)
	}
}

func TestJudgeStreamReportsBeforeAgySeesTheEnd(t *testing.T) {
	s := serve(t, strings.NewReader(stopStream), "")
	buf := make([]byte, len(stopStream))
	if _, err := io.ReadFull(s.resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if len(s.reports) != 0 {
		t.Fatalf("reported before the stream ended: %+v", s.reports)
	}
	if _, err := s.resp.Body.Read(buf); err != io.EOF {
		t.Fatalf("end = %v", err)
	}
	if len(s.reports) != 1 {
		t.Fatalf("reports = %+v", s.reports)
	}
}

func TestStreamInAnUnknownEncodingPassesUnjudged(t *testing.T) {
	s := serve(t, strings.NewReader("zstd bytes"), "zstd")
	if len(s.reports) != 1 || s.reports[0].err != nil {
		t.Fatalf("reports = %+v", s.reports)
	}
	if read, _ := s.readAll(); string(read) != "zstd bytes" {
		t.Fatalf("agy read %q", read)
	}
}
