package agy

import (
	"encoding/json"
	"strings"
)

// ACP session/update payloads, verified against agy acp 1.5.x by realagy:
//
//	agent_message_chunk  {content:{type:"text", text}}   pure deltas
//	agent_thought_chunk  {content:{type:"text", text}}
//	user_message_chunk   {content:{type:"text", text}}   prompt echo / load replay
//	tool_call            {toolCallId, name, title, kind, status, rawInput}
//	tool_call_update     {toolCallId, status, rawOutput, content}
//	usage_update         {used, size}

type updateKind int

const (
	updateIgnored updateKind = iota
	updateText
	updateThought
	updateUserEcho
	updateToolStart
	updateToolDone
	updateToolFailed
	updateContextUsage
)

type mappedUpdate struct {
	kind       updateKind
	text       string
	toolCallID string
	toolName   string
	mcpServer  string
	used, size int64
}

type rawUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Name          string          `json:"name"`
	Title         string          `json:"title"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	RawOutput     json.RawMessage `json:"rawOutput"`
	Used          *int64          `json:"used"`
	Size          *int64          `json:"size"`
}

// Kinds that may be leftovers of background work flushed before the prompt
// echo; status frames always apply.
func isContentUpdate(kind updateKind) bool {
	switch kind {
	case updateText, updateThought, updateToolStart, updateToolDone, updateToolFailed:
		return true
	}
	return false
}

func mapUpdate(raw json.RawMessage, bridgeServer string) mappedUpdate {
	var u rawUpdate
	if err := json.Unmarshal(raw, &u); err != nil {
		return mappedUpdate{}
	}
	switch u.SessionUpdate {
	case "agent_message_chunk":
		return mappedUpdate{kind: updateText, text: chunkText(u.Content)}
	case "agent_thought_chunk":
		return mappedUpdate{kind: updateThought, text: chunkText(u.Content)}
	case "user_message_chunk":
		return mappedUpdate{kind: updateUserEcho}
	case "tool_call":
		if u.ToolCallID == "" {
			return mappedUpdate{}
		}
		name, server := splitToolName(u.Name, u.Title, u.Kind, bridgeServer)
		return mappedUpdate{kind: updateToolStart, toolCallID: u.ToolCallID, toolName: name, mcpServer: server}
	case "tool_call_update":
		if u.ToolCallID == "" {
			return mappedUpdate{}
		}
		name, server := splitToolName(u.Name, "", "", bridgeServer)
		switch u.Status {
		case "completed":
			return mappedUpdate{kind: updateToolDone, toolCallID: u.ToolCallID, toolName: name, mcpServer: server}
		case "failed":
			// After an approval agy reports the approved id as failed with
			// this sentinel while the real work runs under another id.
			if strings.Contains(string(u.RawOutput), "approved but never executed") {
				return mappedUpdate{}
			}
			return mappedUpdate{kind: updateToolFailed, toolCallID: u.ToolCallID, toolName: name, mcpServer: server}
		}
		return mappedUpdate{}
	case "usage_update":
		if u.Used == nil || u.Size == nil || *u.Used < 0 || *u.Size <= 0 {
			return mappedUpdate{}
		}
		return mappedUpdate{kind: updateContextUsage, used: *u.Used, size: *u.Size}
	}
	return mappedUpdate{}
}

func chunkText(content json.RawMessage) string {
	var c struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &c) != nil {
		return ""
	}
	return c.Text
}

// splitToolName separates the bridge's `mcp_<server>_` prefix. Other servers'
// calls count as agy's own tools.
func splitToolName(name, title, kind, bridgeServer string) (tool, server string) {
	prefix := "mcp_" + bridgeServer + "_"
	if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
		return name[len(prefix):], bridgeServer
	}
	if name != "" {
		return name, ""
	}
	if title != "" {
		return title, ""
	}
	return kind, ""
}
