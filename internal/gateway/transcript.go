package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"agyrelay/internal/agy"
	"agyrelay/internal/conv"
)

// A transcript is the client's request split into what the planner needs:
// the system text (never part of the reusable prefix) and the body, with
// one key per message and chained prefix hashes.
//
//	body:    m1   m2   m3   m4
//	keys:    k1   k2   k3   k4
//	hashes:  h0   h1   h2   h3   h4      h[i] = sha256(h[i-1] ‖ 0 ‖ k[i])
//
// A session snapshot with count n and hash h[n] matches every later request
// whose first n body messages are unchanged.
type transcript struct {
	system string
	body   []conv.Message
	keys   []string
	hashes []string
}

func newTranscript(messages []conv.Message) *transcript {
	t := &transcript{}
	var system []string
	for _, m := range messages {
		if m.Role == conv.RoleSystem {
			if text := strings.TrimSpace(m.Text); text != "" {
				system = append(system, text)
			}
			continue
		}
		t.body = append(t.body, m)
	}
	t.system = strings.Join(system, "\n\n")
	t.keys = make([]string, len(t.body))
	for i, m := range t.body {
		t.keys[i] = messageKey(m)
	}
	t.hashes = chainHashes("", t.keys)
	return t
}

// chainHashes extends base (the hash of an empty prefix when "") by keys.
// The result has len(keys)+1 entries; entry 0 is base.
func chainHashes(base string, keys []string) []string {
	if base == "" {
		base = hashString("")
	}
	hashes := make([]string, 0, len(keys)+1)
	hashes = append(hashes, base)
	for _, key := range keys {
		base = hashString(base + "\x00" + key)
		hashes = append(hashes, base)
	}
	return hashes
}

// messageKey fingerprints what a message contributes to the session: role,
// visible text, images, and tool identities. Reasoning is excluded; clients
// drop or rewrite it.
func messageKey(m conv.Message) string {
	parts := []any{string(m.Role), strings.TrimSpace(m.Text)}
	for _, image := range m.Images {
		parts = append(parts, hashString(image.MimeType+":"+image.Data))
	}
	for _, call := range m.ToolCalls {
		parts = append(parts, []string{call.ID, call.Name, canonicalJSON(call.Arguments)})
	}
	if m.Role == conv.RoleTool {
		parts = append(parts, m.ToolCallID)
	}
	raw, _ := json.Marshal(parts)
	return hashString(string(raw))
}

// canonicalJSON re-encodes a JSON value with sorted keys and no whitespace,
// so clients that re-serialize arguments still match.
func canonicalJSON(raw json.RawMessage) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	out, _ := json.Marshal(value)
	return string(out)
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// toolsKey identifies the tool set by name; descriptions and schemas may
// drift without changing what the profile and policy say.
func toolsKey(tools []conv.ToolSpec) string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	slices.Sort(names)
	return hashString(strings.Join(names, "\x00"))
}

// --- prompt rendering ---------------------------------------------------------

const (
	continuationPrompt = "Continue the task from the latest tool results above."
	imageOnlyPrompt    = "See the attached images."
	deltaPrompt        = "Continue the task. The attached transcript delta lists, in order, the messages the client recorded after your last reply (tool results, user messages, other models' replies, or updated client system instructions). Attached images are numbered in delta order. The same tool rules as before apply."
	deltaURI           = "relay://transcript-delta"
	historyPlaceholder = "[Image in original transcript]"
)

// replayBudget bounds a fresh prompt in UTF-8 bytes: agy truncates a single
// USER_INPUT past 192 KB (until the context-unlock patch is ported).
const replayBudget = 160_000

// renderMessage writes one message as transcript text. Images are numbered
// from imageBase+1, or replaced by a placeholder when imageBase < 0.
func renderMessage(m conv.Message, imageBase int) string {
	var refs []string
	for i := range m.Images {
		if imageBase < 0 {
			refs = append(refs, historyPlaceholder)
			continue
		}
		refs = append(refs, fmt.Sprintf("[Attached image %d]", imageBase+i+1))
	}
	content := strings.Join(nonEmpty(append([]string{m.Text}, refs...)), "\n")
	switch m.Role {
	case conv.RoleUser:
		if content == "" {
			return ""
		}
		return "<user>\n" + content + "\n</user>"
	case conv.RoleTool:
		return fmt.Sprintf("<tool-result name=%q id=%q>\n%s\n</tool-result>", m.ToolName, m.ToolCallID, content)
	case conv.RoleAssistant:
		var parts []string
		if text := strings.TrimSpace(m.Text); text != "" {
			parts = append(parts, "<assistant>\n"+text+"\n</assistant>")
		}
		for _, call := range m.ToolCalls {
			parts = append(parts, fmt.Sprintf("<assistant-tool-call name=%q id=%q>\n%s\n</assistant-tool-call>", call.Name, call.ID, canonicalJSON(call.Arguments)))
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func nonEmpty(values []string) []string {
	out := values[:0:0]
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// renderHistory keeps whole messages newest-first within maxBytes and notes
// the omission; only an oversized newest tool result may be cut at its head.
func renderHistory(messages []conv.Message, maxBytes int) string {
	rendered := make([]string, 0, len(messages))
	roles := make([]conv.Role, 0, len(messages))
	for _, m := range messages {
		if text := renderMessage(m, -1); text != "" {
			rendered = append(rendered, text)
			roles = append(roles, m.Role)
		}
	}
	if len(rendered) == 0 {
		return ""
	}
	if joined := strings.Join(rendered, "\n\n"); len(joined) <= maxBytes {
		return "<history>\n" + joined + "\n</history>"
	}
	var kept []string
	used, omitted := 0, 0
	for i := len(rendered) - 1; i >= 0; i-- {
		size := len(rendered[i])
		if len(kept) > 0 {
			size += 2
		}
		if used+size <= maxBytes {
			kept = append([]string{rendered[i]}, kept...)
			used += size
			continue
		}
		if len(kept) == 0 && roles[i] == conv.RoleTool && maxBytes > 1024 {
			kept = append(kept, keepTail(rendered[i], maxBytes))
		}
		omitted = i + 1
		break
	}
	if omitted > 0 {
		kept = append([]string{fmt.Sprintf("[Earlier %d history messages omitted to fit the context replay budget]", omitted)}, kept...)
	}
	return "<history>\n" + strings.Join(kept, "\n\n") + "\n</history>"
}

// keepTail keeps a valid UTF-8 suffix of at most maxBytes behind a marker.
func keepTail(text string, maxBytes int) string {
	const marker = "...[Earlier content omitted]...\n"
	available := maxBytes - len(marker)
	if available <= 0 {
		return ""
	}
	start := max(0, len(text)-available)
	for start < len(text) && text[start]&0xc0 == 0x80 {
		start++
	}
	return marker + text[start:]
}

// freshPrompt is the whole input for a new session: client system prompt,
// bounded history, then the newest user input verbatim with its images.
func freshPrompt(t *transcript, budget int) []agy.ContentBlock {
	body := t.body
	inputStart := len(body)
	if len(body) > 0 && body[len(body)-1].Role == conv.RoleUser {
		inputStart = len(body) - 1
	}
	live := continuationPrompt
	var images []conv.Image
	if inputStart < len(body) {
		input := body[inputStart]
		images = input.Images
		live = input.Text
		if strings.TrimSpace(live) == "" {
			live = imageOnlyPrompt
		}
	}
	var sections []string
	if t.system != "" {
		sections = append(sections, "<client-system>\n"+t.system+"\n</client-system>")
	}
	fixed := len(strings.Join(append(slices.Clone(sections), live), "\n\n"))
	historyBudget := max(0, budget-fixed-256)
	if history := renderHistory(body[:inputStart], historyBudget); history != "" {
		sections = append(sections, history)
	}
	sections = append(sections, live)
	return withImages(strings.Join(sections, "\n\n"), images)
}

// deltaPromptBlocks is the input for a warm session: plain user text when
// only user messages were added and nothing else changed, else a transcript
// delta resource.
func deltaPromptBlocks(delta []conv.Message, systemUpdate string) []agy.ContentBlock {
	onlyUsers := systemUpdate == ""
	for _, m := range delta {
		if m.Role != conv.RoleUser {
			onlyUsers = false
		}
	}
	if onlyUsers {
		var texts []string
		var images []conv.Image
		for _, m := range delta {
			if text := strings.TrimSpace(m.Text); text != "" {
				texts = append(texts, m.Text)
			}
			images = append(images, m.Images...)
		}
		text := strings.Join(texts, "\n\n")
		if text == "" {
			text = imageOnlyPrompt
		}
		return withImages(text, images)
	}

	var parts []string
	if systemUpdate != "" {
		parts = append(parts, "<client-system-update>\n"+systemUpdate+"\n</client-system-update>")
	}
	var images []conv.Image
	for _, m := range delta {
		if text := renderMessage(m, len(images)); text != "" {
			parts = append(parts, text)
		}
		images = append(images, m.Images...)
	}
	blocks := []agy.ContentBlock{{
		Type:     "resource",
		Resource: &agy.Resource{URI: deltaURI, MimeType: "text/markdown", Text: strings.Join(parts, "\n\n")},
	}}
	for _, image := range images {
		blocks = append(blocks, agy.ImageBlock(image.Data, image.MimeType))
	}
	return append(blocks, agy.TextBlock(deltaPrompt))
}

func withImages(text string, images []conv.Image) []agy.ContentBlock {
	blocks := make([]agy.ContentBlock, 0, len(images)+1)
	for _, image := range images {
		blocks = append(blocks, agy.ImageBlock(image.Data, image.MimeType))
	}
	return append(blocks, agy.TextBlock(text))
}

// toolResultText formats the client's tool result for the parked MCP call,
// carrying user messages the client added after it as steering.
func toolResultText(result conv.Message, trailing []conv.Message) string {
	text := result.Text
	var notes []string
	for _, m := range trailing {
		if rendered := renderMessage(m, -1); rendered != "" {
			notes = append(notes, rendered)
		}
	}
	if len(notes) == 0 {
		return text
	}
	if text == "" {
		text = "(no output)"
	}
	return text + "\n\n<client-steering>\nThe user sent a message while this tool was running. Address it immediately:\n" + strings.Join(notes, "\n\n") + "\n</client-steering>"
}
