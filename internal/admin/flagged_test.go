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
	reqJSON := `{"model":"gemini-3.8-flash","request":{"systemInstruction":{"parts":[{"text":"You are opencode."}],"role":"user"},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`
	respJSON := `{"error":{"code":429,"message":"Resource has been exhausted.","status":"RESOURCE_EXHAUSTED"}}`
	err := e.flags.Flag(ctx, gateway.FlaggedPrompt{
		Fingerprint: "abc123", Text: "You are opencode.",
		Request: []byte(reqJSON), Response: []byte(respJSON),
		At: now, Until: now.Add(24 * time.Hour),
		KeyHash: "h1", KeyName: "alice", Account: "main", Session: "S1", Model: "gemini-3.8-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.flags.Flag(ctx, gateway.FlaggedPrompt{
		Fingerprint: "legacy1", Text: "Old prompt only.",
		At: now.Add(-time.Minute), Until: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	r := c.do("GET", "/admin/api/flagged-prompts", nil, nil)
	prompts, _ := r.body["prompts"].([]any)
	if r.status != http.StatusOK || len(prompts) != 2 {
		t.Fatalf("list = %d %s", r.status, r.raw)
	}
	got := prompts[0].(map[string]any)
	if got["fingerprint"] != "abc123" || got["active"] != true || got["hits"] != float64(1) || got["key_name"] != "alice" ||
		got["account"] != "main" || got["model"] != "gemini-3.8-flash" {
		t.Fatalf("prompt = %v", got)
	}
	if strings.Contains(r.raw, "You are opencode") || strings.Contains(r.raw, "RESOURCE_EXHAUSTED") {
		t.Fatal("the list carries the prompt bodies")
	}

	r = c.do("GET", "/admin/api/flagged-prompts/abc123", nil, nil)
	if r.status != http.StatusOK || r.body["text"] != "You are opencode." || r.body["session"] != "S1" {
		t.Fatalf("detail = %d %s", r.status, r.raw)
	}
	call, _ := r.body["call"].(map[string]any)
	respView, _ := call["response"].(map[string]any)
	if call == nil || call["status"] != float64(http.StatusTooManyRequests) || call["session"] != "S1" ||
		call["request"] == nil || respView["text"] != respJSON {
		t.Fatalf("call view = %v", call)
	}

	// Legacy records without a captured upstream call omit call and still serve text.
	r = c.do("GET", "/admin/api/flagged-prompts/legacy1", nil, nil)
	if r.status != http.StatusOK || r.body["text"] != "Old prompt only." || r.body["call"] != nil {
		t.Fatalf("legacy detail = %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/admin/api/flagged-prompts/nope", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("detail of an unknown prompt = %d", r.status)
	}

	if r := c.do("POST", "/admin/api/flagged-prompts/abc123/cancel", nil, nil); r.status != http.StatusOK {
		t.Fatalf("cancel = %d %s", r.status, r.raw)
	}
	if until, _ := e.flags.Flagged("abc123"); time.Now().Before(until) {
		t.Fatal("prompt still flagged after the cancel")
	}
	r = c.do("GET", "/admin/api/flagged-prompts", nil, nil)
	prompts, _ = r.body["prompts"].([]any)
	if len(prompts) != 2 || prompts[0].(map[string]any)["active"] != false {
		t.Fatalf("list after cancel = %s", r.raw)
	}
	if r := c.do("POST", "/admin/api/flagged-prompts/abc123/cancel", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("second cancel = %d", r.status)
	}

	// An ended prompt can be pinned back into refusal, and unpinned.
	if r := c.do("POST", "/admin/api/flagged-prompts/abc123/pin", nil, nil); r.status != http.StatusOK {
		t.Fatalf("pin = %d %s", r.status, r.raw)
	}
	if _, pinned := e.flags.Flagged("abc123"); !pinned {
		t.Fatal("prompt not refused after the pin")
	}
	r = c.do("GET", "/admin/api/flagged-prompts", nil, nil)
	prompts, _ = r.body["prompts"].([]any)
	if got := prompts[0].(map[string]any); got["active"] != true || got["pinned"] != true {
		t.Fatalf("list after pin = %s", r.raw)
	}
	if r := c.do("POST", "/admin/api/flagged-prompts/nope/pin", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("pin of an unknown prompt = %d", r.status)
	}
	if r := c.do("POST", "/admin/api/flagged-prompts/abc123/unpin", nil, nil); r.status != http.StatusOK {
		t.Fatalf("unpin = %d %s", r.status, r.raw)
	}
	if r := c.do("POST", "/admin/api/flagged-prompts/abc123/unpin", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("second unpin = %d", r.status)
	}
	if _, pinned := e.flags.Flagged("abc123"); pinned {
		t.Fatal("prompt pinned after the unpin")
	}

	entries, err := e.st.AuditLog(ctx, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, entry := range entries {
		if entry.Detail == "abc123" {
			counts[entry.Action]++
		}
	}
	if counts["flagged_prompt_cancelled"] != 1 || counts["flagged_prompt_pinned"] != 1 || counts["flagged_prompt_unpinned"] != 1 {
		t.Fatalf("audit entries = %v, want one each of cancelled, pinned, unpinned", counts)
	}
}
