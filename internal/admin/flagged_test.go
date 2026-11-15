package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"agyrelay/internal/gateway"
)

func TestFlaggedPromptsAdminAPI(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := SetPassword(ctx, e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	if r := e.client("203.0.113.9").do("GET", "/admin/api/flagged-prompts", nil, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("list without a session = %d", r.status)
	}
	c := e.client("203.0.113.1")
	c.login(testPassword)

	now := time.Now()
	err := e.flags.Flag(ctx, gateway.FlaggedPrompt{Fingerprint: "abc123", Text: "You are opencode.", At: now, Until: now.Add(24 * time.Hour),
		KeyHash: "h1", KeyName: "alice", Account: "main", Model: "gemini-3.8-flash"})
	if err != nil {
		t.Fatal(err)
	}

	r := c.do("GET", "/admin/api/flagged-prompts", nil, nil)
	prompts, _ := r.body["prompts"].([]any)
	if r.status != http.StatusOK || len(prompts) != 1 {
		t.Fatalf("list = %d %s", r.status, r.raw)
	}
	got := prompts[0].(map[string]any)
	if got["fingerprint"] != "abc123" || got["active"] != true || got["hits"] != float64(1) || got["key_name"] != "alice" ||
		got["account"] != "main" || got["model"] != "gemini-3.8-flash" {
		t.Fatalf("prompt = %v", got)
	}
	if strings.Contains(r.raw, "You are opencode") {
		t.Fatal("the list carries the prompt text")
	}

	r = c.do("GET", "/admin/api/flagged-prompts/abc123/text", nil, nil)
	if r.status != http.StatusOK || r.body["text"] != "You are opencode." {
		t.Fatalf("text = %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/admin/api/flagged-prompts/nope/text", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("text of an unknown prompt = %d", r.status)
	}

	if r := c.do("POST", "/admin/api/flagged-prompts/abc123/cancel", nil, nil); r.status != http.StatusOK {
		t.Fatalf("cancel = %d %s", r.status, r.raw)
	}
	if time.Now().Before(e.flags.FlaggedUntil("abc123")) {
		t.Fatal("prompt still flagged after the cancel")
	}
	r = c.do("GET", "/admin/api/flagged-prompts", nil, nil)
	prompts, _ = r.body["prompts"].([]any)
	if len(prompts) != 1 || prompts[0].(map[string]any)["active"] != false {
		t.Fatalf("list after cancel = %s", r.raw)
	}
	if r := c.do("POST", "/admin/api/flagged-prompts/abc123/cancel", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("second cancel = %d", r.status)
	}

	entries, err := e.st.AuditLog(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := 0
	for _, entry := range entries {
		if entry.Action == "flagged_prompt_cancelled" && entry.Detail == "abc123" {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatalf("audit entries for the cancel = %d, want 1", cancelled)
	}
}
