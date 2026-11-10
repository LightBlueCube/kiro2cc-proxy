// Package openai implements the OpenAI Chat Completions and Responses wire
// formats on top of the protocol-neutral conv model.
//
// Reasoning travels in the fields clients already round-trip: Chat uses
// reasoning_content plus OpenRouter-style reasoning_details, Responses uses
// reasoning items. The gateway's anchor rides as the "encrypted" reasoning
// data (reasoning.encrypted / encrypted_content); it is not encrypted, the
// field is only where clients keep opaque reasoning state.
package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agyrelay/internal/conv"
)

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Tools         []chatTool    `json:"tools"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ReasoningEffort string `json:"reasoning_effort"`
	// Reasoning is OpenRouter's reasoning control.
	Reasoning *struct {
		Effort  string `json:"effort"`
		Exclude bool   `json:"exclude"`
	} `json:"reasoning"`
}

func (r chatRequest) effort() string {
	if r.ReasoningEffort == "" && r.Reasoning != nil {
		return r.Reasoning.Effort
	}
	return r.ReasoningEffort
}

// hideReasoning reports whether the client asked to omit reasoning text.
// The anchor is still sent: it is state, not reasoning.
func (r chatRequest) hideReasoning() bool { return r.Reasoning != nil && r.Reasoning.Exclude }

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	// ReasoningDetails stays raw: other providers' entries have other
	// shapes and must not fail the request.
	ReasoningDetails json.RawMessage `json:"reasoning_details"`
}

// OpenRouter reasoning_details entry types.
const (
	detailText      = "reasoning.text"
	detailEncrypted = "reasoning.encrypted"
	detailFormat    = "agyrelay-v1"
)

// reasoningDetail is one reasoning_details entry. Index orders entries of
// one message; stream deltas with the same index continue an entry.
type reasoningDetail struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Data   string `json:"data,omitempty"`
	Format string `json:"format,omitempty"`
	Index  int    `json:"index"`
}

type chatToolCall struct {
	// Index is set only in stream deltas.
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON object encoded as a string.
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// decodeRequest converts a Chat Completions request. Errors describe the
// offending field for a 400 response.
func decodeRequest(req chatRequest) (conv.Request, error) {
	if req.Model == "" {
		return conv.Request{}, errors.New("model is required")
	}
	out := conv.Request{Model: req.Model, Effort: req.effort()}
	for i, tool := range req.Tools {
		if tool.Type != "function" {
			return conv.Request{}, fmt.Errorf("tools[%d]: only function tools are supported", i)
		}
		out.Tools = append(out.Tools, conv.ToolSpec{Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters})
	}
	// Tool messages do not name their tool; the assistant call does.
	callNames := map[string]string{}
	for i, m := range req.Messages {
		msg, err := decodeMessage(m, callNames)
		if err != nil {
			return conv.Request{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msg)
	}
	return out, nil
}

func decodeMessage(m chatMessage, callNames map[string]string) (conv.Message, error) {
	text, images, err := decodeContent(m.Content)
	if err != nil {
		return conv.Message{}, err
	}
	switch m.Role {
	case "system", "developer":
		if len(images) > 0 {
			return conv.Message{}, errors.New("system messages cannot carry images")
		}
		return conv.Message{Role: conv.RoleSystem, Text: text}, nil
	case "user":
		return conv.Message{Role: conv.RoleUser, Text: text, Images: images}, nil
	case "assistant":
		msg := conv.Message{Role: conv.RoleAssistant, Text: text, Anchors: detailAnchors(m.ReasoningDetails)}
		for j, call := range m.ToolCalls {
			args, err := decodeArguments(call.Function.Arguments)
			if err != nil {
				return conv.Message{}, fmt.Errorf("tool_calls[%d]: %w", j, err)
			}
			callNames[call.ID] = call.Function.Name
			msg.ToolCalls = append(msg.ToolCalls, conv.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: args})
		}
		return msg, nil
	case "tool":
		if m.ToolCallID == "" {
			return conv.Message{}, errors.New("tool messages need tool_call_id")
		}
		return conv.Message{Role: conv.RoleTool, Text: text, Images: images, ToolCallID: m.ToolCallID, ToolName: callNames[m.ToolCallID]}, nil
	}
	return conv.Message{}, fmt.Errorf("unsupported role %q", m.Role)
}

// detailAnchors collects the data of reasoning.encrypted entries. Entries
// it cannot read belong to someone else and are skipped.
func detailAnchors(raw json.RawMessage) []string {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	var anchors []string
	for _, entry := range entries {
		var detail struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if json.Unmarshal(entry, &detail) == nil && detail.Type == detailEncrypted && detail.Data != "" {
			anchors = append(anchors, detail.Data)
		}
	}
	return anchors
}

// decodeArguments accepts the string-encoded JSON object; models and
// clients sometimes send an empty string for no arguments.
func decodeArguments(raw string) (json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, errors.New("function.arguments is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// decodeContent reads a string or an array of text and image parts.
func decodeContent(raw json.RawMessage) (string, []conv.Image, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("content must be a string or an array of parts")
	}
	var texts []string
	var images []conv.Image
	for i, part := range parts {
		switch part.Type {
		case "text":
			texts = append(texts, part.Text)
		case "image_url":
			image, err := decodeImage(part.ImageURL)
			if err != nil {
				return "", nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			images = append(images, image)
		default:
			return "", nil, fmt.Errorf("content[%d]: unsupported part type %q", i, part.Type)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

// decodeImage accepts {url} or a bare url string. Only data: URLs are
// accepted: the relay does not fetch remote URLs on a client's behalf.
func decodeImage(raw json.RawMessage) (conv.Image, error) {
	var url string
	if json.Unmarshal(raw, &url) != nil {
		var object struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			return conv.Image{}, errors.New("image_url must be a string or {url}")
		}
		url = object.URL
	}
	header, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	mimeType, isBase64 := strings.CutSuffix(header, ";base64")
	if !strings.HasPrefix(url, "data:") || !ok || !isBase64 || !strings.HasPrefix(mimeType, "image/") {
		return conv.Image{}, errors.New("image_url must be a base64 data: URL of an image")
	}
	return conv.Image{Data: data, MimeType: mimeType}, nil
}
