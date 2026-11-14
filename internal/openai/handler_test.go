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
	spent  float64
}

func (b *stubBackend) Complete(_ context.Context, _ conv.Caller, req conv.Request) <-chan conv.Event {
	b.got = req
	out := make(chan conv.Event, len(b.events))
	for _, ev := range b.events {
		out <- ev
	}
	close(out)
	return out
}

func (b *stubBackend) Models() []conv.ModelInfo                           { return []conv.ModelInfo{{ID: "m"}} }
func (b *stubBackend) Spent(_ context.Context, _ string) (float64, error) { return b.spent, nil }

func post(t *testing.T, serve func(http.ResponseWriter, *http.Request, conv.Caller), body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	serve(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), conv.Caller{Name: "alice"})
	return rec
}

type sseEvent struct {
	name string
	data map[string]any
}

// readSSE parses an event stream; done reports a Chat [DONE] terminator.
func readSSE(t *testing.T, body string) (events []sseEvent, done bool) {
	t.Helper()
	name := ""
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			name = value
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("bad event data %q: %v", data, err)
		}
		events = append(events, sseEvent{name: name, data: payload})
		name = ""
	}
	return events, done
}

func choiceDelta(chunk map[string]any) map[string]any {
	return chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
}

var replyEvents = []conv.Event{
	{Reasoning: "think"},
	{Text: "hi"},
	{ToolCalls: []conv.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"a":1}`)}}},
	{Anchor: "agr1.x"},
	{Usage: &conv.Usage{PromptTokens: 3, CompletionTokens: 2}},
	{Finish: conv.FinishToolCalls},
}

func TestChatDecodeToolTranscript(t *testing.T) {
	backend := &stubBackend{events: []conv.Event{{Finish: conv.FinishStop}}}
	body := `{"model":"m","reasoning":{"effort":"high"},"messages":[
		{"role":"developer","content":"sys"},
		{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":""}}],
		 "reasoning_details":[{"type":"reasoning.text","text":"t","index":0},{"type":"reasoning.encrypted","data":"agr1.x","index":1},{"type":"reasoning.summary","summary":{"odd":"shape"}},"junk"]},
		{"role":"tool","tool_call_id":"c1","content":"data"}],
		"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]}`
	if rec := post(t, NewHandler(backend).Chat, body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := backend.got
	if got.Effort != "high" || len(got.Messages) != 4 || len(got.Tools) != 1 {
		t.Fatalf("request = %+v", got)
	}
	if got.Messages[0].Role != conv.RoleSystem || got.Messages[1].Images[0].MimeType != "image/png" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	assistant := got.Messages[2]
	if string(assistant.ToolCalls[0].Arguments) != "{}" || got.Messages[3].ToolName != "read" {
		t.Fatalf("tool messages = %+v", got.Messages[2:])
	}
	if len(assistant.Anchors) != 1 || assistant.Anchors[0] != "agr1.x" {
		t.Fatalf("anchors = %v", assistant.Anchors)
	}
}

func TestChatRemoteImageRejected(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	if rec := post(t, NewHandler(&stubBackend{}).Chat, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestChatStream(t *testing.T) {
	backend := &stubBackend{events: replyEvents}
	rec := post(t, NewHandler(backend).Chat, `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d %s", rec.Code, rec.Header())
	}
	events, done := readSSE(t, rec.Body.String())
	if !done || len(events) != 6 {
		t.Fatalf("done=%v events=%v", done, events)
	}
	reasoning := choiceDelta(events[0].data)
	if reasoning["reasoning_content"] != "think" || reasoning["role"] != "assistant" {
		t.Fatalf("reasoning delta = %v", reasoning)
	}
	call := choiceDelta(events[2].data)["tool_calls"].([]any)[0].(map[string]any)
	if call["index"].(float64) != 0 || call["function"].(map[string]any)["arguments"] != `{"a":1}` {
		t.Fatalf("tool call delta = %v", call)
	}
	detail := choiceDelta(events[3].data)["reasoning_details"].([]any)[0].(map[string]any)
	if detail["type"] != "reasoning.encrypted" || detail["data"] != "agr1.x" || detail["index"].(float64) != 1 {
		t.Fatalf("anchor delta = %v", detail)
	}
	if finish := events[4].data["choices"].([]any)[0].(map[string]any)["finish_reason"]; finish != "tool_calls" {
		t.Fatalf("finish = %v", finish)
	}
	if usage := events[5].data["usage"].(map[string]any); usage["total_tokens"].(float64) != 5 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestChatExcludeReasoningKeepsAnchor(t *testing.T) {
	backend := &stubBackend{events: replyEvents}
	rec := post(t, NewHandler(backend).Chat, `{"model":"m","reasoning":{"exclude":true},"messages":[{"role":"user","content":"x"}]}`)
	var body struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	message := body.Choices[0].Message
	if _, ok := message["reasoning_content"]; ok {
		t.Fatalf("reasoning not excluded: %v", message)
	}
	details := message["reasoning_details"].([]any)
	if len(details) != 1 || details[0].(map[string]any)["data"] != "agr1.x" {
		t.Fatalf("details = %v", details)
	}
}

func TestEarlyErrorKeepsStatus(t *testing.T) {
	backend := &stubBackend{events: []conv.Event{{Err: &conv.Error{Kind: conv.ErrQuota, Message: "quota", RetryAfterSeconds: 60}}}}
	h := NewHandler(backend)
	for _, serve := range []func(http.ResponseWriter, *http.Request, conv.Caller){h.Chat, h.Responses} {
		rec := post(t, serve, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}],"input":"x"}`)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
			t.Fatalf("status %d headers %v", rec.Code, rec.Header())
		}
	}

	unknown := NewHandler(&stubBackend{events: []conv.Event{{Err: &conv.Error{Kind: conv.ErrUnknownModel, Message: "no model"}}}})
	for _, serve := range []func(http.ResponseWriter, *http.Request, conv.Caller){unknown.Chat, unknown.Responses} {
		rec := post(t, serve, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}],"input":"x"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(rec.Body.String(), `"code":"model_not_found"`) {
			t.Fatalf("unknown model = %d %s", rec.Code, rec.Body.String())
		}
	}

	cases := []struct {
		name      string
		err       *conv.Error
		wantCode  int
		wantType  string
		wantError string
	}{
		{
			name:      "key spend limit is 403 non-retryable",
			err:       &conv.Error{Kind: conv.ErrKeyLimit, Message: "spent $10"},
			wantCode:  http.StatusForbidden,
			wantType:  "insufficient_quota",
			wantError: "insufficient_quota",
		},
		{
			name:      "key worker limit is 429 retryable",
			err:       &conv.Error{Kind: conv.ErrKeyBusy, Message: "concurrent limit"},
			wantCode:  http.StatusTooManyRequests,
			wantType:  "rate_limit_exceeded",
			wantError: "rate_limit_exceeded",
		},
		{
			name:      "long quota exhaustion is 403 insufficient_quota",
			err:       &conv.Error{Kind: conv.ErrQuota, HTTPStatus: http.StatusForbidden, Message: "Individual quota reached"},
			wantCode:  http.StatusForbidden,
			wantType:  "insufficient_quota",
			wantError: "insufficient_quota",
		},
		{
			name:      "cancelled request is 499 non-retryable",
			err:       &conv.Error{Kind: conv.ErrCancelled, Message: "cancelled"},
			wantCode:  statusClientClosed,
			wantType:  "request_cancelled",
			wantError: "request_cancelled",
		},
		{
			name:      "upstream 422 passes status through",
			err:       &conv.Error{Kind: conv.ErrInvalidRequest, HTTPStatus: http.StatusUnprocessableEntity, Message: "unprocessable"},
			wantCode:  http.StatusUnprocessableEntity,
			wantType:  "invalid_request_error",
			wantError: "invalid_request_error",
		},
		{
			name:      "upstream 413 passes status through",
			err:       &conv.Error{Kind: conv.ErrContextLength, HTTPStatus: http.StatusRequestEntityTooLarge, Message: "too large"},
			wantCode:  http.StatusRequestEntityTooLarge,
			wantType:  "invalid_request_error",
			wantError: "context_length_exceeded",
		},
		{
			name:      "upstream 503 passes status through",
			err:       &conv.Error{Kind: conv.ErrUpstream, HTTPStatus: http.StatusServiceUnavailable, Message: "HTTP 503 Service Unavailable"},
			wantCode:  http.StatusServiceUnavailable,
			wantType:  "upstream_error",
			wantError: "upstream_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewHandler(&stubBackend{events: []conv.Event{{Err: tc.err}}})
			for _, serve := range []func(http.ResponseWriter, *http.Request, conv.Caller){handler.Chat, handler.Responses} {
				rec := post(t, serve, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}],"input":"x"}`)
				if rec.Code != tc.wantCode || rec.Header().Get("Retry-After") != "" {
					t.Fatalf("status=%d retry-after=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
				}
				var body struct {
					Error struct {
						Type string `json:"type"`
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Type != tc.wantType || body.Error.Code != tc.wantError {
					t.Fatalf("error body = %+v, want type=%q code=%q", body.Error, tc.wantType, tc.wantError)
				}
			}
		})
	}
}

func TestResponsesDecodeGroupsAssistantItems(t *testing.T) {
	backend := &stubBackend{events: []conv.Event{{Finish: conv.FinishStop}}}
	body := `{"model":"m","instructions":"sys","reasoning":{"effort":"low","summary":"auto"},
		"tools":[{"type":"function","name":"read","parameters":{"type":"object"},"strict":false}],
		"input":[
		{"role":"user","content":"look"},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"t"}]},
		{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"on it","annotations":[]}]},
		{"type":"function_call","id":"fc_1","call_id":"c1","name":"read","arguments":"{\"a\":1}"},
		{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"agr1.x"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"data"},{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,AAAA"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`
	if rec := post(t, NewHandler(backend).Responses, body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	m := backend.got.Messages
	if backend.got.Effort != "low" || len(backend.got.Tools) != 1 || len(m) != 5 {
		t.Fatalf("request = %+v", backend.got)
	}
	if m[0].Role != conv.RoleSystem || m[1].Text != "look" {
		t.Fatalf("head = %+v", m[:2])
	}
	assistant := m[2]
	if assistant.Role != conv.RoleAssistant || assistant.Text != "on it" || len(assistant.ToolCalls) != 1 || assistant.Anchors[0] != "agr1.x" {
		t.Fatalf("assistant = %+v", assistant)
	}
	if m[3].Role != conv.RoleTool || m[3].ToolName != "read" || m[3].Text != "data" || len(m[3].Images) != 1 || m[4].Text != "next" {
		t.Fatalf("tail = %+v", m[3:])
	}
}

func TestResponsesRejectsServerState(t *testing.T) {
	h := NewHandler(&stubBackend{})
	for _, body := range []string{
		`{"model":"m","previous_response_id":"resp_1","input":"x"}`,
		`{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`,
		`{"model":"m","input":"x","tools":[{"type":"web_search"}]}`,
	} {
		if rec := post(t, h.Responses, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", body, rec.Code)
		}
	}
}

func TestResponsesNonStream(t *testing.T) {
	rec := post(t, NewHandler(&stubBackend{events: replyEvents}).Responses, `{"model":"m","input":"x"}`)
	var body struct {
		Status string           `json:"status"`
		Output []map[string]any `json:"output"`
		Usage  map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	types := []string{}
	for _, item := range body.Output {
		types = append(types, item["type"].(string))
	}
	if body.Status != "completed" || strings.Join(types, ",") != "reasoning,message,function_call,reasoning" {
		t.Fatalf("status %s output %v", body.Status, types)
	}
	if body.Output[1]["content"].([]any)[0].(map[string]any)["text"] != "hi" || body.Output[2]["call_id"] != "c1" || body.Output[3]["encrypted_content"] != "agr1.x" {
		t.Fatalf("output = %v", body.Output)
	}
	if body.Usage["total_tokens"].(float64) != 5 {
		t.Fatalf("usage = %v", body.Usage)
	}
}

func TestResponsesStreamEventOrder(t *testing.T) {
	rec := post(t, NewHandler(&stubBackend{events: replyEvents}).Responses, `{"model":"m","stream":true,"input":"x"}`)
	events, _ := readSSE(t, rec.Body.String())
	var names []string
	for i, ev := range events {
		if ev.data["type"] != ev.name || ev.data["sequence_number"].(float64) != float64(i) {
			t.Fatalf("event %d = %s %v", i, ev.name, ev.data)
		}
		names = append(names, ev.name)
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done",
		"response.output_item.added", "response.output_item.done",
		"response.completed",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s", strings.Join(names, "\n"))
	}
	final := events[len(events)-1].data["response"].(map[string]any)
	if final["status"] != "completed" || len(final["output"].([]any)) != 4 {
		t.Fatalf("final = %v", final)
	}
	anchorDone := events[19].data["item"].(map[string]any)
	if anchorDone["encrypted_content"] != "agr1.x" {
		t.Fatalf("anchor item = %v", anchorDone)
	}
}

func TestResponsesStreamFailureAfterOutput(t *testing.T) {
	events := []conv.Event{{Text: "partial"}, {Err: &conv.Error{Kind: conv.ErrUpstream, Message: "agy exited"}}}
	rec := post(t, NewHandler(&stubBackend{events: events}).Responses, `{"model":"m","stream":true,"input":"x"}`)
	parsed, _ := readSSE(t, rec.Body.String())
	last := parsed[len(parsed)-1]
	response := last.data["response"].(map[string]any)
	if last.name != "response.failed" || response["status"] != "failed" || response["error"].(map[string]any)["message"] != "agy exited" {
		t.Fatalf("last = %v", last)
	}
}

func TestBillingSubscriptionAndUsage(t *testing.T) {
	h := NewHandler(&stubBackend{spent: 1.25})
	totalLimit := 50.0
	cases := []struct {
		caller conv.Caller
		want   float64
	}{
		{caller: conv.Caller{Name: "unlimited"}, want: unlimitedBillingUSD},
		{caller: conv.Caller{Name: "total", LimitTotal: &totalLimit}, want: 50},
		{caller: conv.Caller{Name: "groups", Limits: map[string]float64{"gemini": 10, "claude-gpt": 15}}, want: 25},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.BillingSubscription(rec, httptest.NewRequest(http.MethodGet, "/v1/dashboard/billing/subscription", nil), tc.caller)
		var sub map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
			t.Fatal(err)
		}
		if sub["object"] != "billing_subscription" || sub["has_payment_method"] != true ||
			sub["soft_limit_usd"] != tc.want || sub["hard_limit_usd"] != tc.want || sub["system_hard_limit_usd"] != tc.want ||
			sub["access_until"] != float64(0) {
			t.Fatalf("%s subscription = %v", tc.caller.Name, sub)
		}
	}

	rec := httptest.NewRecorder()
	h.BillingUsage(rec, httptest.NewRequest(http.MethodGet, "/v1/dashboard/billing/usage?start_date=2026-11-01&end_date=2026-11-15", nil), conv.Caller{KeyHash: "h1"})
	var usage map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage["object"] != "list" || usage["total_usage"] != float64(125) {
		t.Fatalf("usage = %v", usage)
	}
}
