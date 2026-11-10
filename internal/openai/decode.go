// Package openai implements the OpenAI Chat Completions wire format on top
// of the protocol-neutral conv model.
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
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
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
	out := conv.Request{Model: req.Model, Effort: req.ReasoningEffort}
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
		msg := conv.Message{Role: conv.RoleAssistant, Text: text}
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
