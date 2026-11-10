package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agyrelay/internal/conv"
)

type stubBackend struct {
	got    conv.Request
	events []conv.Event
}

func (b *stubBackend) Complete(_ context.Context, _ string, req conv.Request) <-chan conv.Event {
	b.got = req
	out := make(chan conv.Event, len(b.events))
	for _, ev := range b.events {
		out <- ev
	}
	close(out)
	return out
}

func (b *stubBackend) Models() []conv.ModelInfo { return []conv.ModelInfo{{ID: "m"}} }

func post(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	h.Chat(rec, req, "alice")
	return rec
}

func TestDecodeToolTranscript(t *testing.T) {
	backend := &stubBackend{events: []conv.Event{{Finish: conv.FinishStop}}}
	body := `{"model":"m","reasoning_effort":"high","messages":[
		{"role":"developer","content":"sys"},
		{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":""}}]},
		{"role":"tool","tool_call_id":"c1","content":"data"}],
		"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]}`
	if rec := post(NewHandler(backend), body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := backend.got
	if got.Effort != "high" || len(got.Messages) != 4 || len(got.Tools) != 1 {
		t.Fatalf("request = %+v", got)
	}
	if got.Messages[0].Role != conv.RoleSystem || got.Messages[1].Images[0].MimeType != "image/png" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if string(got.Messages[2].ToolCalls[0].Arguments) != "{}" || got.Messages[3].ToolName != "read" {
		t.Fatalf("tool messages = %+v", got.Messages[2:])
	}
}

func TestRemoteImageRejected(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	if rec := post(NewHandler(&stubBackend{}), body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestStreamToolCallsAndUsage(t *testing.T) {
	backend := &stubBackend{events: []conv.Event{
		{Reasoning: "hidden"},
		{Text: "hi"},
		{ToolCalls: []conv.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"a":1}`)}}},
		{Usage: &conv.Usage{PromptTokens: 3, CompletionTokens: 2}},
		{Finish: conv.FinishToolCalls},
	}}
	rec := post(NewHandler(backend), `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d %s", rec.Code, rec.Header())
	}
	var chunks []map[string]any
	scanner := bufio.NewScanner(rec.Body)
	done := false
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	if !done || len(chunks) != 4 {
		t.Fatalf("done=%v chunks=%v", done, chunks)
	}
	if strings.Contains(rec.Body.String(), "hidden") {
		t.Fatal("reasoning leaked into the stream")
	}
	delta := chunks[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	call := delta["tool_calls"].([]any)[0].(map[string]any)
	if call["index"].(float64) != 0 || call["function"].(map[string]any)["arguments"] != `{"a":1}` {
		t.Fatalf("tool call chunk = %v", delta)
	}
	if finish := chunks[2]["choices"].([]any)[0].(map[string]any)["finish_reason"]; finish != "tool_calls" {
		t.Fatalf("finish = %v", finish)
	}
	if usage := chunks[3]["usage"].(map[string]any); usage["total_tokens"].(float64) != 5 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestEarlyErrorKeepsStatus(t *testing.T) {
	backend := &stubBackend{events: []conv.Event{{Err: &conv.Error{Kind: conv.ErrQuota, Message: "quota", RetryAfterSeconds: 60}}}}
	rec := post(NewHandler(backend), `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
}
