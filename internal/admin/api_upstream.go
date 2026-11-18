package admin

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agyrelay/internal/store"
)

// upstreamInfoView is one captured generation call of a request, without
// its bodies.
type upstreamInfoView struct {
	ID                int64     `json:"id"`
	At                time.Time `json:"at"`
	Account           string    `json:"account"`
	Session           string    `json:"session"`
	Seq               int       `json:"seq"`
	Model             string    `json:"model"`
	Status            int       `json:"status"`
	RequestBytes      int64     `json:"request_bytes"`
	ResponseBytes     int64     `json:"response_bytes"`
	ResponseTruncated bool      `json:"response_truncated"`
}

// listUpstreamCalls serves the generation calls agy made for a request.
func (h *Handler) listUpstreamCalls(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "request id must be a number")
		return
	}
	calls, err := h.store.UpstreamCalls(r.Context(), id)
	if err != nil {
		h.internalError(w, "list upstream calls", err)
		return
	}
	out := []upstreamInfoView{}
	for _, c := range calls {
		out = append(out, upstreamInfoView{
			ID: c.ID, At: c.At, Account: cmp.Or(c.AccountName, c.Account), Session: c.Session, Seq: c.Seq, Model: c.Model, Status: c.Status,
			RequestBytes: c.RequestBytes, ResponseBytes: c.ResponseBytes, ResponseTruncated: c.ResponseTruncated,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// upstreamCallView is a generation call with its request as agy sent it
// and its response as Google answered.
type upstreamCallView struct {
	upstreamInfoView
	// Request is the JSON body sent; RequestText holds it when it is not
	// JSON.
	Request     json.RawMessage `json:"request,omitempty"`
	RequestText string          `json:"request_text,omitempty"`
	Response    responseView    `json:"response"`
}

// responseView is a response with its stream chunks merged.
type responseView struct {
	// Parts are the first candidate's content parts; neighboring text
	// parts of one kind are joined.
	Parts        []map[string]any `json:"parts"`
	FinishReason string           `json:"finish_reason,omitempty"`
	Usage        json.RawMessage  `json:"usage,omitempty"`
	// Skipped counts stream chunks that were not JSON.
	Skipped int `json:"skipped,omitempty"`
	// Text is the whole body when it holds no readable chunk, as it does
	// for an error status.
	Text string `json:"text,omitempty"`
}

func (h *Handler) getUpstreamCall(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "call id must be a number")
		return
	}
	c, err := h.store.UpstreamCall(r.Context(), id)
	if err != nil {
		h.fail(w, "read upstream call", err)
		return
	}
	writeJSON(w, http.StatusOK, buildUpstreamCallView(c))
}

func buildUpstreamCallView(c store.UpstreamCall) upstreamCallView {
	v := upstreamCallView{
		upstreamInfoView: upstreamInfoView{
			ID: c.ID, At: c.At, Account: cmp.Or(c.AccountName, c.Account), Session: c.Session, Seq: c.Seq, Model: c.Model, Status: c.Status,
			RequestBytes: int64(len(c.Request)), ResponseBytes: int64(len(c.Response)), ResponseTruncated: c.ResponseTruncated,
		},
		Response: parseResponse(c.Response, c.Status),
	}
	if json.Valid(c.Request) {
		v.Request = c.Request
	} else {
		v.RequestText = string(c.Request)
	}
	return v
}

// streamChunk is one chunk of a generation response; v1internal wraps the
// body in "response".
type streamChunk struct {
	chunkBody
	Response *chunkBody `json:"response"`
}

type chunkBody struct {
	Candidates []struct {
		Content struct {
			Parts []map[string]any `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata json.RawMessage `json:"usageMetadata"`
}

// parseResponse merges the chunks of a response body, sent as server-sent
// events, a JSON array, or one JSON object. A body without a readable
// chunk, or with an error status, is kept as text.
func parseResponse(body []byte, status int) responseView {
	view := responseView{Parts: []map[string]any{}}
	if status/100 != 2 {
		view.Text = string(body)
		return view
	}
	chunks, skipped := splitChunks(body)
	view.Skipped = skipped
	if len(chunks) == 0 {
		view.Text = string(body)
		return view
	}
	for _, raw := range chunks {
		var chunk streamChunk
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&chunk); err != nil {
			view.Skipped++
			continue
		}
		part := chunk.chunkBody
		if chunk.Response != nil {
			part = *chunk.Response
		}
		if len(part.UsageMetadata) > 0 && string(part.UsageMetadata) != "null" {
			view.Usage = part.UsageMetadata
		}
		if len(part.Candidates) == 0 {
			continue
		}
		candidate := part.Candidates[0]
		if candidate.FinishReason != "" {
			view.FinishReason = candidate.FinishReason
		}
		for _, p := range candidate.Content.Parts {
			view.Parts = appendPart(view.Parts, p)
		}
	}
	return view
}

// splitChunks cuts a body into the JSON values of its chunks and counts
// the stream lines that carried none.
func splitChunks(body []byte) (chunks []json.RawMessage, skipped int) {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("[")) {
		if err := json.Unmarshal(trimmed, &chunks); err == nil {
			return chunks, 0
		}
		return nil, 0
	}
	if bytes.HasPrefix(trimmed, []byte("{")) && json.Valid(trimmed) {
		return []json.RawMessage{trimmed}, 0
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(nil, len(body)+1)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(scanner.Text(), "data:")
		payload = strings.TrimSpace(payload)
		if !ok || payload == "" || payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			skipped++
			continue
		}
		chunks = append(chunks, json.RawMessage(payload))
	}
	return chunks, skipped
}

// appendPart adds p, joining it to the last part when both are text of
// the same kind (thought or answer).
func appendPart(parts []map[string]any, p map[string]any) []map[string]any {
	if n := len(parts); n > 0 && joinable(parts[n-1], p) {
		last := parts[n-1]
		last["text"] = last["text"].(string) + p["text"].(string)
		if sig, ok := p["thoughtSignature"]; ok {
			last["thoughtSignature"] = sig
		}
		return parts
	}
	return append(parts, p)
}

func joinable(a, b map[string]any) bool {
	return isText(a) && isText(b) && a["thought"] == b["thought"]
}

// isText reports whether the part is only text, with an optional thought
// flag and signature.
func isText(p map[string]any) bool {
	if _, ok := p["text"].(string); !ok {
		return false
	}
	for key := range p {
		if key != "text" && key != "thought" && key != "thoughtSignature" {
			return false
		}
	}
	return true
}
