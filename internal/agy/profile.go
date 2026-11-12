package agy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Custom agent profile: replaces agy's whole system prompt and removes its
// native tools. Facts verified live by realagy against agy 1.5.x:
//
//   - agy acp only appends to its system prompt over ACP
//     (_meta.antigravity.systemPrompt.append); `--agent NAME` selects
//     <gemini-dir>/config/agents/NAME.md for the whole process;
//   - excludeDefaultComponents drops agy's prompt sections and its file,
//     shell, subagent, and scheduling tools;
//   - under a custom agent ACP-provided MCP tools become lazy, permission-
//     gated call_mcp_tool calls unless an mcpServers entry with the same
//     server name marks each tool eager (no wildcard);
//   - only Markdown agents and the list form of mcpServers are loaded.

// BridgeServer is the MCP server name client tools are served under; agy
// exposes them to the model as mcp_<BridgeServer>_<tool>.
const BridgeServer = "client"

const profilePrefix = "agyrelay-"

// Marks files agyrelay owns.
const profileDescription = "agyrelay API bridge profile, written by agyrelay for its agy acp processes. Safe to delete."

// profilePrompt is static so agy's prompt cache holds across sessions; the
// client's own system prompt follows it in the per-session append.
const profilePrompt = `You are the model behind an API client. The Antigravity CLI only runs you; the client owns the conversation, the tools, and how replies are shown.

- The client's system prompt, if any, follows these rules. The earlier conversation arrives in the prompt inside <history>; <system> entries there and in later transcript deltas are system messages the client added at that point. Continue the conversation as the client's assistant.
- Your tools are the client's tools, served by the client MCP server. Use them for all file, shell, and other work; the client executes every call and applies its own permissions.
- Do the work yourself, in the foreground: do not start background tasks.`

// Profile is an agent profile for one client tool set.
type Profile struct {
	Name    string
	Content string
}

// NewProfile renders the profile for the given eager tool names. The name
// hashes the content, so different tool sets never overwrite each other.
func NewProfile(toolNames []string) Profile {
	tools := slices.Clone(toolNames)
	slices.Sort(tools)
	tools = slices.Compact(tools)
	digest := sha256.Sum256([]byte(profileMarkdown("", tools)))
	name := profilePrefix + hex.EncodeToString(digest[:])[:12]
	return Profile{Name: name, Content: profileMarkdown(name, tools)}
}

func profileMarkdown(name string, tools []string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", yamlString(name))
	fmt.Fprintf(&b, "description: %s\n", yamlString(profileDescription))
	b.WriteString("hidden: true\nexcludeDefaultComponents: true\nmcpServers:\n")
	fmt.Fprintf(&b, "  - name: %s\n", BridgeServer)
	if len(tools) > 0 {
		b.WriteString("    tools:\n")
		for _, tool := range tools {
			fmt.Fprintf(&b, "      %s:\n        eager: true\n", yamlString(tool))
		}
	}
	b.WriteString("---\n")
	b.WriteString(profilePrompt)
	b.WriteString("\n")
	return b.String()
}

// JSON strings are valid YAML double-quoted scalars.
func yamlString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// Install writes the profile into <geminiDir>/config/agents atomically
// (agy re-reads agents for every session). An unchanged file is left alone.
func (p Profile) Install(geminiDir string) error {
	dir := filepath.Join(geminiDir, "config", "agents")
	file := filepath.Join(dir, p.Name+".md")
	if current, err := os.ReadFile(file); err == nil && string(current) == p.Content {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create agent dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, p.Name+".*.tmp")
	if err != nil {
		return fmt.Errorf("write agent profile: %w", err)
	}
	if _, err := tmp.WriteString(p.Content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write agent profile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write agent profile: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("install agent profile: %w", err)
	}
	return nil
}
