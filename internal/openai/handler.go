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
	Complete(ctx context.Context, user string, req conv.Request) <-chan conv.Event
	Models() []conv.ModelInfo
}

type Handler struct {
	backend Backend
}

func NewHandler(backend Backend) *Handler { return &Handler{backend: backend} }

// Models serves GET /v1/models.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := []model{}
	for _, m := range h.backend.Models() {
		data = append(data, model{ID: m.ID, Object: "model", OwnedBy: "antigravity"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// Chat serves POST /v1/chat/completions for the authenticated user.
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request, user string) {
	var wire chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&wire); err != nil {
		WriteError(w, &conv.Error{Kind: conv.ErrInvalidRequest, Message: "invalid JSON body: " + err.Error()})
		return
	}
	req, err := decodeRequest(wire)
	if err != nil {
		WriteError(w, &conv.Error{Kind: conv.ErrInvalidRequest, Message: err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events := h.backend.Complete(ctx, user, req)
	meta := responseMeta{id: "chatcmpl-" + rand.Text(), model: wire.Model, created: time.Now().Unix()}
	if wire.Stream {
		includeUsage := wire.StreamOptions != nil && wire.StreamOptions.IncludeUsage
		stream(w, events, meta, includeUsage)
		return
	}
	collect(w, events, meta)
}

type responseMeta struct {
	id      string
	model   string
	created int64
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

// collect answers a non-streaming request. Reasoning is not passed on
// until its wire format is designed.
func collect(w http.ResponseWriter, events <-chan conv.Event, meta responseMeta) {
	var text strings.Builder
	var calls []conv.ToolCall
	var usage *conv.Usage
	for ev := range events {
		switch {
		case ev.Err != nil:
			WriteError(w, ev.Err)
			return
		case ev.Text != "":
			text.WriteString(ev.Text)
		case ev.ToolCalls != nil:
			calls = append(calls, ev.ToolCalls...)
		case ev.Usage != nil:
			usage = ev.Usage
		case ev.Finish != "":
			message := map[string]any{"role": "assistant", "content": text.String()}
			if len(calls) > 0 {
				message["tool_calls"] = encodeToolCalls(calls, false, 0)
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

// sse writes chat.completion.chunk events.
type sse struct {
	w       http.ResponseWriter
	flusher http.Flusher
	meta    responseMeta
	started bool
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

func (s *sse) data(payload any) {
	s.begin()
	raw, _ := json.Marshal(payload)
	fmt.Fprintf(s.w, "data: %s\n\n", raw)
	s.flusher.Flush()
}

func (s *sse) comment() {
	s.begin()
	fmt.Fprint(s.w, ": keepalive\n\n")
	s.flusher.Flush()
}

func (s *sse) chunk(delta map[string]any, finish *string) {
	s.data(map[string]any{
		"id": s.meta.id, "object": "chat.completion.chunk", "created": s.meta.created, "model": s.meta.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	})
}

// stream answers a streaming request. Headers wait for the first event, so
// a failure before any output still gets a proper HTTP status; after a
// heartbeat interval without output the stream starts anyway.
func stream(w http.ResponseWriter, events <-chan conv.Event, meta responseMeta, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, &conv.Error{Kind: conv.ErrUpstream, Message: "streaming unsupported by the HTTP server"})
		return
	}
	s := &sse{w: w, flusher: flusher, meta: meta}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	roleSent := false
	toolIndex := 0
	var usage *conv.Usage
	delta := func(fields map[string]any) {
		if !roleSent {
			fields["role"] = "assistant"
			roleSent = true
		}
		s.chunk(fields, nil)
	}
	for {
		var ev conv.Event
		select {
		case <-ticker.C:
			s.comment()
			continue
		case next, open := <-events:
			if !open {
				return // request context ended
			}
			ev = next
		}
		switch {
		case ev.Err != nil:
			if !s.started {
				WriteError(w, ev.Err)
				return
			}
			s.data(errorBody(ev.Err))
			return
		case ev.Text != "":
			delta(map[string]any{"content": ev.Text})
		case ev.ToolCalls != nil:
			delta(map[string]any{"tool_calls": encodeToolCalls(ev.ToolCalls, true, toolIndex)})
			toolIndex += len(ev.ToolCalls)
		case ev.Usage != nil:
			usage = ev.Usage
		case ev.Finish != "":
			if !roleSent {
				delta(map[string]any{"content": ""})
			}
			finish := string(ev.Finish)
			s.chunk(map[string]any{}, &finish)
			if includeUsage && usage != nil {
				s.data(map[string]any{
					"id": meta.id, "object": "chat.completion.chunk", "created": meta.created, "model": meta.model,
					"choices": []any{}, "usage": encodeUsage(usage),
				})
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
	}
}

func statusFor(kind conv.ErrorKind) (int, string) {
	switch kind {
	case conv.ErrInvalidRequest:
		return http.StatusBadRequest, "invalid_request_error"
	case conv.ErrUnauthorized:
		return http.StatusUnauthorized, "invalid_api_key"
	case conv.ErrUnknownModel:
		return http.StatusNotFound, "model_not_found"
	case conv.ErrQuota:
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	case conv.ErrOverloaded, conv.ErrUnavailable:
		return http.StatusServiceUnavailable, "service_unavailable"
	}
	return http.StatusBadGateway, "upstream_error"
}

func errorBody(err *conv.Error) map[string]any {
	_, code := statusFor(err.Kind)
	return map[string]any{"error": map[string]any{"message": err.Message, "type": code, "code": code}}
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
