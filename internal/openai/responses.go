package openai

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"time"

	"agyrelay/internal/conv"
)

// Output item shapes. Slices are never nil so empty lists encode as [].
type reasoningItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Summary          []summaryPart `json:"summary"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
	Status           string        `json:"status"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messageItem struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Role    string       `json:"role"`
	Status  string       `json:"status"`
	Content []outputText `json:"content"`
}

type outputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

const (
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusFailed     = "failed"
)

func itemID(prefix string) string { return prefix + rand.Text() }

// responseBuilder turns gateway events into output items, and into the
// Responses streaming events when emit is set. One item is open at a time:
// reasoning text, then reply text, then calls, each closed before the next.
type responseBuilder struct {
	id      string
	model   string
	created int64
	tools   json.RawMessage
	emit    func(name string, payload map[string]any)

	seq    int
	output []any
	open   any // *reasoningItem or *messageItem being streamed
	usage  *conv.Usage
}

func (b *responseBuilder) send(name string, payload map[string]any) {
	if b.emit == nil {
		return
	}
	payload["type"] = name
	payload["sequence_number"] = b.seq
	b.seq++
	b.emit(name, payload)
}

func (b *responseBuilder) index() int { return len(b.output) - 1 }

// add appends item and announces it.
func (b *responseBuilder) add(item any) {
	b.output = append(b.output, item)
	b.send("response.output_item.added", map[string]any{"output_index": b.index(), "item": item})
}

func (b *responseBuilder) reasoning(delta string) {
	item, ok := b.open.(*reasoningItem)
	if !ok {
		b.closeOpen()
		item = &reasoningItem{Type: "reasoning", ID: itemID("rs_"), Summary: []summaryPart{}, Status: statusInProgress}
		b.add(item)
		item.Summary = append(item.Summary, summaryPart{Type: "summary_text"})
		b.send("response.reasoning_summary_part.added", map[string]any{"item_id": item.ID, "output_index": b.index(), "summary_index": 0, "part": summaryPart{Type: "summary_text"}})
		b.open = item
	}
	item.Summary[0].Text += delta
	b.send("response.reasoning_summary_text.delta", map[string]any{"item_id": item.ID, "output_index": b.index(), "summary_index": 0, "delta": delta})
}

func (b *responseBuilder) text(delta string) {
	item, ok := b.open.(*messageItem)
	if !ok {
		b.closeOpen()
		item = &messageItem{Type: "message", ID: itemID("msg_"), Role: "assistant", Content: []outputText{}, Status: statusInProgress}
		b.add(item)
		part := outputText{Type: "output_text", Annotations: []any{}}
		item.Content = append(item.Content, part)
		b.send("response.content_part.added", map[string]any{"item_id": item.ID, "output_index": b.index(), "content_index": 0, "part": part})
		b.open = item
	}
	item.Content[0].Text += delta
	b.send("response.output_text.delta", map[string]any{"item_id": item.ID, "output_index": b.index(), "content_index": 0, "delta": delta, "logprobs": []any{}})
}

// closeOpen completes the item being streamed.
func (b *responseBuilder) closeOpen() {
	switch item := b.open.(type) {
	case *reasoningItem:
		part := item.Summary[0]
		b.send("response.reasoning_summary_text.done", map[string]any{"item_id": item.ID, "output_index": b.index(), "summary_index": 0, "text": part.Text})
		b.send("response.reasoning_summary_part.done", map[string]any{"item_id": item.ID, "output_index": b.index(), "summary_index": 0, "part": part})
		item.Status = statusCompleted
	case *messageItem:
		part := item.Content[0]
		b.send("response.output_text.done", map[string]any{"item_id": item.ID, "output_index": b.index(), "content_index": 0, "text": part.Text, "logprobs": []any{}})
		b.send("response.content_part.done", map[string]any{"item_id": item.ID, "output_index": b.index(), "content_index": 0, "part": part})
		item.Status = statusCompleted
	default:
		return
	}
	b.send("response.output_item.done", map[string]any{"output_index": b.index(), "item": b.open})
	b.open = nil
}

func (b *responseBuilder) toolCalls(calls []conv.ToolCall) {
	b.closeOpen()
	for _, call := range calls {
		item := &functionCallItem{Type: "function_call", ID: itemID("fc_"), CallID: call.ID, Name: call.Name, Status: statusInProgress}
		b.add(item)
		item.Arguments = string(call.Arguments)
		b.send("response.function_call_arguments.delta", map[string]any{"item_id": item.ID, "output_index": b.index(), "delta": item.Arguments})
		b.send("response.function_call_arguments.done", map[string]any{"item_id": item.ID, "output_index": b.index(), "arguments": item.Arguments})
		item.Status = statusCompleted
		b.send("response.output_item.done", map[string]any{"output_index": b.index(), "item": item})
	}
}

// anchor appends a reasoning item without summary that carries the anchor
// as encrypted_content, where clients keep opaque reasoning state.
func (b *responseBuilder) anchor(value string) {
	b.closeOpen()
	item := &reasoningItem{Type: "reasoning", ID: itemID("rs_"), Summary: []summaryPart{}, Status: statusInProgress}
	b.add(item)
	item.EncryptedContent = value
	item.Status = statusCompleted
	b.send("response.output_item.done", map[string]any{"output_index": b.index(), "item": item})
}

// response is the response object in its current state.
func (b *responseBuilder) response(status string, failure *conv.Error) map[string]any {
	output := b.output
	if output == nil {
		output = []any{}
	}
	tools := b.tools
	if len(tools) == 0 || string(tools) == "null" {
		tools = json.RawMessage("[]")
	}
	body := map[string]any{
		"id": b.id, "object": "response", "created_at": b.created, "status": status, "model": b.model,
		"output": output, "parallel_tool_calls": true, "tool_choice": "auto", "tools": tools,
		"store": false, "error": nil, "incomplete_details": nil,
	}
	if status == statusIncomplete {
		body["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
	}
	if failure != nil {
		_, code := statusFor(failure.Kind)
		body["error"] = map[string]string{"code": code, "message": failure.Message}
	}
	if b.usage != nil {
		body["usage"] = encodeResponsesUsage(b.usage)
	}
	return body
}

func encodeResponsesUsage(u *conv.Usage) map[string]any {
	return map[string]any{
		"input_tokens":          u.PromptTokens,
		"input_tokens_details":  map[string]int64{"cached_tokens": u.CachedTokens},
		"output_tokens":         u.CompletionTokens,
		"output_tokens_details": map[string]int64{"reasoning_tokens": u.ReasoningTokens},
		"total_tokens":          u.PromptTokens + u.CompletionTokens,
	}
}

// apply feeds one non-final event; it returns false for Finish and Err.
func (b *responseBuilder) apply(ev conv.Event) bool {
	switch {
	case ev.Text != "":
		b.text(ev.Text)
	case ev.Reasoning != "":
		b.reasoning(ev.Reasoning)
	case ev.ToolCalls != nil:
		b.toolCalls(ev.ToolCalls)
	case ev.Anchor != "":
		b.anchor(ev.Anchor)
	case ev.Usage != nil:
		b.usage = ev.Usage
	default:
		return false
	}
	return true
}

func finishStatus(finish conv.FinishReason) string {
	if finish == conv.FinishLength {
		return statusIncomplete
	}
	return statusCompleted
}

// Responses serves POST /v1/responses for the authenticated caller.
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request, caller conv.Caller) {
	var wire responsesRequest
	if !decodeBody(w, r, &wire) {
		return
	}
	req, err := decodeResponsesRequest(wire)
	if err != nil {
		WriteError(w, &conv.Error{Kind: conv.ErrInvalidRequest, Message: err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events := h.backend.Complete(ctx, caller, req)
	b := &responseBuilder{id: itemID("resp_"), model: wire.Model, created: time.Now().Unix(), tools: wire.Tools}
	if wire.Stream {
		streamResponses(w, events, b)
		return
	}
	for ev := range events {
		if b.apply(ev) {
			continue
		}
		if ev.Err != nil {
			WriteError(w, ev.Err)
			return
		}
		b.closeOpen()
		writeJSON(w, http.StatusOK, b.response(finishStatus(ev.Finish), nil))
		return
	}
}

// streamResponses sends the Responses event stream. Until the first event
// is written an error still becomes an HTTP error response.
func streamResponses(w http.ResponseWriter, events <-chan conv.Event, b *responseBuilder) {
	s, ok := newSSE(w)
	if !ok {
		return
	}
	b.emit = func(name string, payload map[string]any) { s.write(name, payload) }
	announced := false
	announce := func() {
		if announced {
			return
		}
		announced = true
		b.send("response.created", map[string]any{"response": b.response(statusInProgress, nil)})
		b.send("response.in_progress", map[string]any{"response": b.response(statusInProgress, nil)})
	}
	pump(s, events, func(ev conv.Event) bool {
		if ev.Err != nil {
			if !s.started {
				WriteError(w, ev.Err)
				return false
			}
			announce()
			b.closeOpen()
			b.send("response.failed", map[string]any{"response": b.response(statusFailed, ev.Err)})
			return false
		}
		announce()
		if b.apply(ev) {
			return true
		}
		b.closeOpen()
		status := finishStatus(ev.Finish)
		name := "response.completed"
		if status == statusIncomplete {
			name = "response.incomplete"
		}
		b.send(name, map[string]any{"response": b.response(status, nil)})
		return false
	})
}
