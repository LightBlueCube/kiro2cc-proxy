// Package conv holds the protocol-neutral conversation model shared by the
// API codecs and the gateway.
package conv

import (
	"encoding/json"
	"slices"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Image is inline base64 image data.
type Image struct {
	Data     string
	MimeType string
}

// ToolCall is a call the assistant made.
type ToolCall struct {
	ID   string
	Name string
	// Arguments is a JSON object.
	Arguments json.RawMessage
}

// Message is one transcript entry.
type Message struct {
	Role   Role
	Text   string
	Images []Image
	// ToolCalls are the assistant's calls (RoleAssistant).
	ToolCalls []ToolCall
	// ToolCallID and ToolName identify the call a RoleTool message answers.
	ToolCallID string
	ToolName   string
	// Anchors are opaque values the client returned with an assistant
	// message (Event.Anchor of an earlier response). Codecs carry them
	// without interpreting them.
	Anchors []string
}

// ToolSpec is a client tool offered to the model.
type ToolSpec struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object.
	Parameters json.RawMessage
}

// Request is one completion request.
type Request struct {
	// API names the endpoint ("chat", "responses") and Stream whether the
	// client streams; both only label records.
	API    string
	Stream bool
	Model  string
	// Effort is the requested reasoning effort ("" = default).
	Effort   string
	Messages []Message
	Tools    []ToolSpec
}

// FinishReason ends a response.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
)

// Usage is token accounting for a response.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	ReasoningTokens  int64
	CachedTokens     int64
}

// Event is one item of a response stream. Exactly one field group is set.
type Event struct {
	Text      string
	Reasoning string
	ToolCalls []ToolCall
	// Anchor is an opaque value the client should return with this
	// response's assistant message; it is the last event before Finish.
	Anchor string
	Usage  *Usage
	// Finish ends a successful response.
	Finish FinishReason
	// Err ends a failed response.
	Err *Error
}

// ErrorKind classifies failures for the HTTP layer.
type ErrorKind int

const (
	ErrInvalidRequest ErrorKind = iota + 1
	ErrUnauthorized
	ErrForbidden
	ErrUnknownModel
	ErrQuota
	ErrOverloaded
	ErrUnavailable
	ErrUpstream
)

type Error struct {
	Kind    ErrorKind
	Message string
	// RetryAfterSeconds hints when the request may succeed (0 = unknown).
	RetryAfterSeconds int
}

func (e *Error) Error() string { return e.Message }

// Caller is the API key a request came with.
type Caller struct {
	// Name identifies the key; sessions and records are scoped by it.
	Name string
	// Models limits the key to these model ids; empty allows all.
	Models []string
}

// Allows reports whether the caller may use the model id.
func (c Caller) Allows(modelID string) bool {
	return len(c.Models) == 0 || slices.Contains(c.Models, modelID)
}

// ModelInfo is one model the gateway serves.
type ModelInfo struct {
	ID      string
	Name    string
	Efforts []string
}
