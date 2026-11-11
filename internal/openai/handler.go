package openai

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agyrelay/internal/conv"
)

const (
	maxRequestBytes = 64 << 20
	// heartbeat keeps proxies and clients from timing out while agy works
	// silently; an SSE comment line is ignored by clients.
	heartbeat = 15 * time.Second
)

// Backend runs completions.
type Backend interface {
	Complete(ctx context.Context, caller conv.Caller, req conv.Request) <-chan conv.Event
	Models() []conv.ModelInfo
}

type Handler struct {
	backend Backend
}

func NewHandler(backend Backend) *Handler { return &Handler{backend: backend} }

// Models serves GET /v1/models: the models the caller may use.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request, caller conv.Caller) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := []model{}
	for _, m := range h.backend.Models() {
		if caller.Allows(m.ID) {
			data = append(data, model{ID: m.ID, Object: "model", OwnedBy: "antigravity"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// decodeBody reads a JSON request body into v, answering 400 on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(v); err != nil {
		WriteError(w, &conv.Error{Kind: conv.ErrInvalidRequest, Message: "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

// Chat serves POST /v1/chat/completions for the authenticated caller.
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request, caller conv.Caller) {
	var wire chatRequest
	if !decodeBody(w, r, &wire) {
		return
	}
	req, err := decodeRequest(wire)
	if err != nil {
		WriteError(w, &conv.Error{Kind: conv.ErrInvalidRequest, Message: err.Error()})
		return
	}
	req.API, req.Stream = "chat", wire.Stream
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events := h.backend.Complete(ctx, caller, req)
	meta := chatMeta{id: "chatcmpl-" + rand.Text(), model: wire.Model, created: time.Now().Unix(), hideReasoning: wire.hideReasoning()}
	if wire.Stream {
		includeUsage := wire.StreamOptions != nil && wire.StreamOptions.IncludeUsage
		streamChat(w, events, meta, includeUsage)
		return
	}
	collectChat(w, events, meta)
}

type chatMeta struct {
	id            string
	model         string
	created       int64
	hideReasoning bool
}

type usageJSON struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func encodeUsage(u *conv.Usage) *usageJSON {
	out := &usageJSON{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.PromptTokens + u.CompletionTokens}
	out.PromptTokensDetails.CachedTokens = u.CachedTokens
	out.CompletionTokensDetails.ReasoningTokens = u.ReasoningTokens
	return out
}

func encodeToolCalls(calls []conv.ToolCall, indexed bool, base int) []chatToolCall {
	out := make([]chatToolCall, len(calls))
	for i, call := range calls {
		out[i] = chatToolCall{ID: call.ID, Type: "function", Function: chatFunction{Name: call.Name, Arguments: string(call.Arguments)}}
		if indexed {
			index := base + i
			out[i].Index = &index
		}
	}
	return out
}

// reasoning_details indexes: the text entry, then the anchor entry.
const (
	textDetailIndex   = 0
	anchorDetailIndex = 1
)

func anchorDetail(anchor string) reasoningDetail {
	return reasoningDetail{Type: detailEncrypted, Data: anchor, Format: detailFormat, Index: anchorDetailIndex}
}

// collectChat answers a non-streaming request.
func collectChat(w http.ResponseWriter, events <-chan conv.Event, meta chatMeta) {
	var text, reasoning strings.Builder
	var calls []conv.ToolCall
	var usage *conv.Usage
	anchor := ""
	for ev := range events {
		switch {
		case ev.Err != nil:
			WriteError(w, ev.Err)
			return
		case ev.Text != "":
			text.WriteString(ev.Text)
		case ev.Reasoning != "":
			reasoning.WriteString(ev.Reasoning)
		case ev.ToolCalls != nil:
			calls = append(calls, ev.ToolCalls...)
		case ev.Anchor != "":
			anchor = ev.Anchor
		case ev.Usage != nil:
			usage = ev.Usage
		case ev.Finish != "":
			message := map[string]any{"role": "assistant", "content": text.String()}
			if len(calls) > 0 {
				message["tool_calls"] = encodeToolCalls(calls, false, 0)
			}
			var details []reasoningDetail
			if reasoning.Len() > 0 && !meta.hideReasoning {
				message["reasoning_content"] = reasoning.String()
				details = append(details, reasoningDetail{Type: detailText, Text: reasoning.String(), Format: detailFormat, Index: textDetailIndex})
			}
			if anchor != "" {
				details = append(details, anchorDetail(anchor))
			}
			if len(details) > 0 {
				message["reasoning_details"] = details
			}
			body := map[string]any{
				"id": meta.id, "object": "chat.completion", "created": meta.created, "model": meta.model,
				"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": string(ev.Finish)}},
			}
			if usage != nil {
				body["usage"] = encodeUsage(usage)
			}
			writeJSON(w, http.StatusOK, body)
			return
		}
	}
	// The backend ends every stream with Finish or Err; reaching here
	// means the request context ended and nobody is listening.
}

// sse writes server-sent events. Headers wait for the first write, so a
// failure before any output can still get a proper HTTP status.
type sse struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
}

func newSSE(w http.ResponseWriter) (*sse, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, &conv.Error{Kind: conv.ErrUpstream, Message: "streaming unsupported by the HTTP server"})
		return nil, false
	}
	return &sse{w: w, flusher: flusher}, true
}

func (s *sse) begin() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
}

// write sends one event; name is empty for Chat's unnamed data events.
func (s *sse) write(name string, payload any) {
	s.begin()
	raw, _ := json.Marshal(payload)
	if name != "" {
		fmt.Fprintf(s.w, "event: %s\n", name)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", raw)
	s.flusher.Flush()
}

func (s *sse) raw(line string) {
	s.begin()
	fmt.Fprint(s.w, line)
	s.flusher.Flush()
}

func (s *sse) comment() { s.raw(": keepalive\n\n") }

// pump reads events, sending a keepalive comment whenever none arrived for
// a heartbeat interval. handle returns false to stop. pump returns when
// events closes (the request context ended).
func pump(s *sse, events <-chan conv.Event, handle func(conv.Event) bool) {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.comment()
		case ev, open := <-events:
			if !open || !handle(ev) {
				return
			}
		}
	}
}

// streamChat answers a streaming request with chat.completion.chunk events.
func streamChat(w http.ResponseWriter, events <-chan conv.Event, meta chatMeta, includeUsage bool) {
	s, ok := newSSE(w)
	if !ok {
		return
	}
	chunk := func(delta map[string]any, finish *string) {
		s.write("", map[string]any{
			"id": meta.id, "object": "chat.completion.chunk", "created": meta.created, "model": meta.model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		})
	}
	roleSent := false
	delta := func(fields map[string]any) {
		if !roleSent {
			fields["role"] = "assistant"
			roleSent = true
		}
		chunk(fields, nil)
	}
	toolIndex := 0
	var usage *conv.Usage
	pump(s, events, func(ev conv.Event) bool {
		switch {
		case ev.Err != nil:
			if !s.started {
				WriteError(w, ev.Err)
				return false
			}
			s.write("", errorBody(ev.Err))
			return false
		case ev.Text != "":
			delta(map[string]any{"content": ev.Text})
		case ev.Reasoning != "":
			if !meta.hideReasoning {
				delta(map[string]any{
					"reasoning_content": ev.Reasoning,
					"reasoning_details": []reasoningDetail{{Type: detailText, Text: ev.Reasoning, Format: detailFormat, Index: textDetailIndex}},
				})
			}
		case ev.ToolCalls != nil:
			delta(map[string]any{"tool_calls": encodeToolCalls(ev.ToolCalls, true, toolIndex)})
			toolIndex += len(ev.ToolCalls)
		case ev.Anchor != "":
			delta(map[string]any{"reasoning_details": []reasoningDetail{anchorDetail(ev.Anchor)}})
		case ev.Usage != nil:
			usage = ev.Usage
		case ev.Finish != "":
			if !roleSent {
				delta(map[string]any{"content": ""})
			}
			finish := string(ev.Finish)
			chunk(map[string]any{}, &finish)
			if includeUsage && usage != nil {
				s.write("", map[string]any{
					"id": meta.id, "object": "chat.completion.chunk", "created": meta.created, "model": meta.model,
					"choices": []any{}, "usage": encodeUsage(usage),
				})
			}
			s.raw("data: [DONE]\n\n")
			return false
		}
		return true
	})
}

func statusFor(kind conv.ErrorKind) (int, string) {
	switch kind {
	case conv.ErrInvalidRequest:
		return http.StatusBadRequest, "invalid_request_error"
	case conv.ErrContextLength:
		// OpenAI's code; clients compact the conversation on it.
		return http.StatusBadRequest, "context_length_exceeded"
	case conv.ErrUnauthorized:
		return http.StatusUnauthorized, "invalid_api_key"
	case conv.ErrForbidden:
		return http.StatusForbidden, "permission_denied"
	case conv.ErrUnknownModel:
		return http.StatusNotFound, "model_not_found"
	case conv.ErrQuota:
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	case conv.ErrKeyLimit:
		return http.StatusTooManyRequests, "insufficient_quota"
	case conv.ErrOverloaded, conv.ErrUnavailable:
		return http.StatusServiceUnavailable, "service_unavailable"
	}
	return http.StatusBadGateway, "upstream_error"
}

func errorBody(err *conv.Error) map[string]any {
	_, code := statusFor(err.Kind)
	errType := code
	if err.Kind == conv.ErrContextLength {
		errType = "invalid_request_error" // as OpenAI types it
	}
	return map[string]any{"error": map[string]any{"message": err.Message, "type": errType, "code": code}}
}

// WriteError writes err as an OpenAI error response.
func WriteError(w http.ResponseWriter, err *conv.Error) {
	status, _ := statusFor(err.Kind)
	if err.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(err.RetryAfterSeconds))
	}
	writeJSON(w, status, errorBody(err))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A write error means the client left; there is no one to report it to.
	_ = json.NewEncoder(w).Encode(body)
}
