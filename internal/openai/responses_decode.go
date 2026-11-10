package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agyrelay/internal/conv"
)

// responsesRequest is the part of POST /v1/responses the relay serves. The
// relay is stateless: clients send the whole conversation every time.
type responsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	Tools              json.RawMessage `json:"tools"`
	Stream             bool            `json:"stream"`
	PreviousResponseID string          `json:"previous_response_id"`
	Reasoning          *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// inputItem is the union of input item shapes the relay reads. Unknown
// fields are ignored so clients may send back items verbatim.
type inputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// reasoning
	EncryptedContent string `json:"encrypted_content"`
	// function_call and function_call_output
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

type inputPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Refusal  string          `json:"refusal"`
	ImageURL json.RawMessage `json:"image_url"`
	FileID   string          `json:"file_id"`
}

var errStateful = errors.New("this relay is stateless: send the full conversation as input items (store: false) instead of previous_response_id or item_reference")

func decodeResponsesRequest(req responsesRequest) (conv.Request, error) {
	if req.Model == "" {
		return conv.Request{}, errors.New("model is required")
	}
	if req.PreviousResponseID != "" {
		return conv.Request{}, errStateful
	}
	out := conv.Request{Model: req.Model}
	if req.Reasoning != nil {
		out.Effort = req.Reasoning.Effort
	}
	tools, err := decodeResponsesTools(req.Tools)
	if err != nil {
		return conv.Request{}, err
	}
	out.Tools = tools
	if strings.TrimSpace(req.Instructions) != "" {
		out.Messages = append(out.Messages, conv.Message{Role: conv.RoleSystem, Text: req.Instructions})
	}
	messages, err := decodeInput(req.Input)
	if err != nil {
		return conv.Request{}, err
	}
	out.Messages = append(out.Messages, messages...)
	return out, nil
}

func decodeResponsesTools(raw json.RawMessage) ([]conv.ToolSpec, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var tools []responsesTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	out := make([]conv.ToolSpec, 0, len(tools))
	for i, tool := range tools {
		if tool.Type != "function" {
			return nil, fmt.Errorf("tools[%d]: only function tools are supported, not %q", i, tool.Type)
		}
		out = append(out, conv.ToolSpec{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return out, nil
}

// decodeInput turns input items into messages. Consecutive assistant-side
// items (assistant messages, reasoning, function calls) form one assistant
// message, the way the relay produced them:
//
//	reasoning, message(assistant), function_call, function_call  →  assistant{text, 2 calls}
//	function_call_output, function_call_output                   →  tool, tool
func decodeInput(raw json.RawMessage) ([]conv.Message, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []conv.Message{{Role: conv.RoleUser, Text: text}}, nil
	}
	var items []inputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("input must be a string or an array of items")
	}
	d := inputDecoder{callNames: map[string]string{}}
	for i, item := range items {
		if err := d.add(item); err != nil {
			return nil, fmt.Errorf("input[%d]: %w", i, err)
		}
	}
	d.flush()
	return d.out, nil
}

type inputDecoder struct {
	out       []conv.Message
	assistant *conv.Message
	callNames map[string]string
}

// current is the open assistant message.
func (d *inputDecoder) current() *conv.Message {
	if d.assistant == nil {
		d.assistant = &conv.Message{Role: conv.RoleAssistant}
	}
	return d.assistant
}

func (d *inputDecoder) flush() {
	if d.assistant != nil {
		d.out = append(d.out, *d.assistant)
		d.assistant = nil
	}
}

func (d *inputDecoder) add(item inputItem) error {
	kind := item.Type
	if kind == "" && item.Role != "" {
		kind = "message"
	}
	switch kind {
	case "message":
		return d.message(item)
	case "reasoning":
		if item.EncryptedContent != "" {
			m := d.current()
			m.Anchors = append(m.Anchors, item.EncryptedContent)
		}
		return nil
	case "function_call":
		args, err := decodeArguments(item.Arguments)
		if err != nil {
			return err
		}
		if item.CallID == "" || item.Name == "" {
			return errors.New("function_call needs call_id and name")
		}
		d.callNames[item.CallID] = item.Name
		m := d.current()
		m.ToolCalls = append(m.ToolCalls, conv.ToolCall{ID: item.CallID, Name: item.Name, Arguments: args})
		return nil
	case "function_call_output":
		if item.CallID == "" {
			return errors.New("function_call_output needs call_id")
		}
		text, images, err := decodeParts(item.Output)
		if err != nil {
			return err
		}
		d.flush()
		d.out = append(d.out, conv.Message{Role: conv.RoleTool, Text: text, Images: images, ToolCallID: item.CallID, ToolName: d.callNames[item.CallID]})
		return nil
	case "item_reference":
		return errStateful
	}
	return fmt.Errorf("unsupported item type %q", item.Type)
}

func (d *inputDecoder) message(item inputItem) error {
	text, images, err := decodeParts(item.Content)
	if err != nil {
		return err
	}
	switch item.Role {
	case "assistant":
		if len(images) > 0 {
			return errors.New("assistant messages cannot carry images")
		}
		// The relay's own output text is split only where reasoning
		// interrupts it, so pieces join without a separator.
		m := d.current()
		m.Text += text
		return nil
	case "user":
		d.flush()
		d.out = append(d.out, conv.Message{Role: conv.RoleUser, Text: text, Images: images})
		return nil
	case "system", "developer":
		if len(images) > 0 {
			return errors.New("system messages cannot carry images")
		}
		d.flush()
		d.out = append(d.out, conv.Message{Role: conv.RoleSystem, Text: text})
		return nil
	}
	return fmt.Errorf("unsupported role %q", item.Role)
}

// decodeParts reads a string or a list of input/output content parts.
func decodeParts(raw json.RawMessage) (string, []conv.Image, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var parts []inputPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("content must be a string or an array of parts")
	}
	var texts []string
	var images []conv.Image
	for i, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			texts = append(texts, part.Text)
		case "refusal":
			texts = append(texts, part.Refusal)
		case "input_image":
			if part.FileID != "" {
				return "", nil, fmt.Errorf("content[%d]: file_id images are not supported; send a data: URL", i)
			}
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
