package gateway

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"agyrelay/internal/agy"
	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
)

// Bridge-internal tools. They are listed next to the client's tools but
// answered by the gateway; the client never sees them.
const (
	parallelToolName = "parallel"
	maxParallelCalls = 16
)

var parallelToolSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"calls":{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"object","properties":{"name":{"type":"string","minLength":1},"arguments":{"type":"object","additionalProperties":true}},"required":["name"],"additionalProperties":false}}},"required":["calls"],"additionalProperties":false}`, maxParallelCalls))

const parallelToolDescription = "Run 1-16 independent client tools in one batch. The client runs them and returns the results in input order. Do not use for dependent or conflicting work or nested parallel calls."

func isInternalTool(name string) bool { return name == parallelToolName }

// bridgeTools lists the client's tools plus the internal one.
func bridgeTools(tools []conv.ToolSpec) []bridge.Tool {
	out := make([]bridge.Tool, 0, len(tools)+1)
	for _, tool := range tools {
		out = append(out, bridge.Tool{Name: tool.Name, Description: tool.Description, InputSchema: normalizeSchema(tool.Parameters)})
	}
	if len(tools) > 0 {
		out = append(out, bridge.Tool{Name: parallelToolName, Description: parallelToolDescription, InputSchema: parallelToolSchema})
	}
	return out
}

// IdleProfile is the profile of requests without client tools; account
// probes use its process.
func IdleProfile() agy.Profile { return profileFor(nil) }

// profileFor is the agent profile whose eager tool list matches tools.
func profileFor(tools []conv.ToolSpec) agy.Profile {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if len(tools) > 0 {
		names = append(names, parallelToolName)
	}
	return agy.NewProfile(names)
}

// toolPolicy is appended to agy's system prompt for every session: it names
// the client tools and how agy exposes them.
func toolPolicy(tools []conv.ToolSpec) string {
	if len(tools) == 0 {
		return "The client offers no tools in this session: answer in text."
	}
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return strings.Join([]string{
		fmt.Sprintf("The client's tools in this session, served by the %s MCP server (you call them as mcp_%s_<name>; the transcript names them without the prefix):", agy.BridgeServer, agy.BridgeServer),
		strings.Join(names, ", "),
		fmt.Sprintf("For two or more independent, non-conflicting client tool calls, call mcp_%s_%s once with {calls: [{name, arguments}, ...]}, names without the prefix, instead of issuing them sequentially. Never nest %s.", agy.BridgeServer, parallelToolName, parallelToolName),
	}, "\n")
}

// normalizeSchema makes a client schema acceptable to agy's model
// transports: an object at the top level, const as single-value enum,
// string-literal anyOf as enum, and disjoint primitive anyOf as oneOf.
func normalizeSchema(raw json.RawMessage) json.RawMessage {
	var schema map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil || schema == nil {
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
	}
	normalized, _ := normalizeNode(schema).(map[string]any)
	if normalized["type"] != "object" {
		normalized = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
	}
	out, err := json.Marshal(normalized)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
	}
	return out
}

var (
	schemaMapKeys    = []string{"properties", "patternProperties", "$defs", "definitions"}
	schemaListKeys   = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
	schemaSingleKeys = []string{"items", "additionalProperties", "not", "if", "then", "else", "contains"}
	primitiveTypes   = []string{"string", "boolean", "object", "array", "null", "number", "integer"}
)

func normalizeNode(value any) any {
	source, ok := value.(map[string]any)
	if !ok {
		return value
	}
	_, hasEnum := source["enum"]
	result := map[string]any{}
	for key, entry := range source {
		switch {
		case key == "const" && !hasEnum:
			result["enum"] = []any{entry}
		case slices.Contains(schemaMapKeys, key):
			children, ok := entry.(map[string]any)
			if !ok {
				result[key] = entry
				continue
			}
			mapped := map[string]any{}
			for name, child := range children {
				mapped[name] = normalizeNode(child)
			}
			result[key] = mapped
		case slices.Contains(schemaListKeys, key):
			list, ok := entry.([]any)
			if !ok {
				result[key] = entry
				continue
			}
			mapped := make([]any, len(list))
			for i, child := range list {
				mapped[i] = normalizeNode(child)
			}
			result[key] = mapped
		case slices.Contains(schemaSingleKeys, key):
			result[key] = normalizeNode(entry)
		default:
			result[key] = entry
		}
	}
	normalizeAnyOf(source, result)
	return result
}

// normalizeAnyOf rewrites anyOf branches agy's Claude transport rejects.
func normalizeAnyOf(source, result map[string]any) {
	branches, ok := source["anyOf"].([]any)
	if !ok || len(branches) == 0 {
		return
	}
	_, hasType := source["type"]
	_, hasEnum := source["enum"]
	var literals []any
	var rest []any
	for _, branch := range branches {
		if literal, ok := stringLiteral(branch); ok {
			literals = append(literals, literal)
			continue
		}
		rest = append(rest, branch)
	}
	if !hasType && !hasEnum && len(rest) == 0 {
		delete(result, "anyOf")
		result["type"] = "string"
		result["enum"] = literals
		return
	}
	merged := make([]any, 0, len(rest)+1)
	if len(literals) > 0 {
		merged = append(merged, map[string]any{"type": "string", "enum": literals})
	}
	for _, branch := range rest {
		merged = append(merged, normalizeNode(branch))
	}
	// Distinct primitive types cannot overlap, so oneOf is equivalent.
	seen := map[string]bool{}
	for _, branch := range merged {
		node, _ := branch.(map[string]any)
		typ, _ := node["type"].(string)
		if !slices.Contains(primitiveTypes, typ) || seen[typ] {
			return
		}
		seen[typ] = true
	}
	if seen["number"] && seen["integer"] {
		return
	}
	if _, hasOneOf := source["oneOf"]; hasOneOf {
		return
	}
	result["oneOf"] = merged
	delete(result, "anyOf")
}

// stringLiteral matches {const: "x"} or {type: "string", const: "x"}.
func stringLiteral(branch any) (string, bool) {
	node, ok := branch.(map[string]any)
	if !ok {
		return "", false
	}
	literal, ok := node["const"].(string)
	if !ok {
		return "", false
	}
	for key, value := range node {
		if key == "const" {
			continue
		}
		if key != "type" || value != "string" {
			return "", false
		}
	}
	return literal, true
}

type parallelCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// parseParallel validates a parallel tool call's arguments.
func parseParallel(args json.RawMessage, known func(string) bool) ([]parallelCall, error) {
	var input struct {
		Calls []parallelCall `json:"calls"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(args)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("parallel accepts only {calls: [{name, arguments}]}: %v", err)
	}
	if len(input.Calls) < 1 || len(input.Calls) > maxParallelCalls {
		return nil, fmt.Errorf("parallel.calls must contain 1-%d tool calls", maxParallelCalls)
	}
	for i, call := range input.Calls {
		name := strings.TrimSpace(call.Name)
		if name == "" {
			return nil, fmt.Errorf("parallel.calls[%d].name must be a non-empty string", i)
		}
		if isInternalTool(name) || !known(name) {
			return nil, fmt.Errorf("parallel.calls[%d] cannot invoke %s", i, name)
		}
		if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
			input.Calls[i].Arguments = json.RawMessage("{}")
			continue
		}
		var object map[string]any
		if json.Unmarshal(call.Arguments, &object) != nil {
			return nil, fmt.Errorf("parallel.calls[%d].arguments must be an object", i)
		}
	}
	return input.Calls, nil
}

type parallelEntry struct {
	CallID  string          `json:"callId"`
	Name    string          `json:"name"`
	IsError bool            `json:"isError"`
	Content []parallelBlock `json:"content"`
}

type parallelBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Index    int    `json:"index,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// formatParallel aggregates a batch's results in input order; images are
// referenced by their 1-based position among the attached images.
func formatParallel(calls []*pendingCall, results []bridge.Result) bridge.Result {
	var images []bridge.Image
	entries := make([]parallelEntry, len(calls))
	isError := false
	for i, call := range calls {
		r := results[i]
		blocks := []parallelBlock{{Type: "text", Text: r.Text}}
		for _, image := range r.Images {
			images = append(images, image)
			blocks = append(blocks, parallelBlock{Type: "image", Index: len(images), MimeType: image.MimeType})
		}
		entries[i] = parallelEntry{CallID: call.id, Name: call.name, IsError: r.IsError, Content: blocks}
		isError = isError || r.IsError
	}
	text, _ := json.MarshalIndent(map[string]any{"results": entries}, "", "  ")
	return bridge.Result{Text: string(text), Images: images, IsError: isError}
}
