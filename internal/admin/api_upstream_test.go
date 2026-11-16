package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"agyrelay/internal/store"
)

func TestParseResponseJoinsStreamChunks(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"response": {"candidates": [{"content": {"role": "model", "parts": [{"text": "Let me ", "thought": true}]}}]}}`,
		``,
		`data: {"response": {"candidates": [{"content": {"parts": [{"text": "think.", "thought": true, "thoughtSignature": "sig"}]}}]}}`,
		``,
		`data: {"response": {"candidates": [{"content": {"parts": [{"text": "Hel"}]}}]}}`,
		``,
		`data: {"response": {"candidates": [{"content": {"parts": [{"text": "lo"}, {"functionCall": {"name": "read", "args": {"n": 12345678901234567890}}}]}, "finishReason": "STOP"}], "usageMetadata": {"promptTokenCount": 7}}}`,
		``,
		`data: not json`,
		`data: [DONE]`,
	}, "\n")
	got := parseResponse([]byte(sse), http.StatusOK)

	want := []map[string]any{
		{"text": "Let me think.", "thought": true, "thoughtSignature": "sig"},
		{"text": "Hello"},
		{"functionCall": map[string]any{"name": "read", "args": map[string]any{"n": json.Number("12345678901234567890")}}},
	}
	if !reflect.DeepEqual(got.Parts, want) {
		t.Fatalf("parts = %#v", got.Parts)
	}
	if got.FinishReason != "STOP" || string(got.Usage) != `{"promptTokenCount": 7}` || got.Skipped != 1 || got.Text != "" {
		t.Fatalf("view = %+v", got)
	}
}

func TestParseResponseTakesOtherShapes(t *testing.T) {
	chunk := `{"candidates": [{"content": {"parts": [{"text": "a"}]}}]}`
	for name, body := range map[string]string{
		"one object":       chunk,
		"array":            "[" + chunk + "," + chunk + "]",
		"data without gap": "data:" + chunk,
	} {
		got := parseResponse([]byte(body), http.StatusOK)
		text, _ := got.Parts[0]["text"].(string)
		if text == "" || got.Text != "" {
			t.Errorf("%s: view = %+v", name, got)
		}
	}

	if got := parseResponse([]byte(`{"error": {"code": 429}}`), http.StatusTooManyRequests); len(got.Parts) != 0 || got.Text != `{"error": {"code": 429}}` {
		t.Errorf("error status: %+v", got)
	}
	if got := parseResponse([]byte("<html>"), http.StatusOK); len(got.Parts) != 0 || got.Text != "<html>" {
		t.Errorf("unreadable body: %+v", got)
	}
	if got := parseResponse(nil, http.StatusOK); len(got.Parts) != 0 || got.Text != "" {
		t.Errorf("empty body: %+v", got)
	}
}

func TestUpstreamCallsAreServedToTheAdminOnly(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	if err := e.st.AddRequest(ctx, store.Request{At: now, Account: "main", Status: store.StatusOK, CallToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddUpstreamCall(ctx, store.UpstreamCall{
		At: now, CallToken: "tok", Account: "main", Session: "S1", Seq: 3, Model: "m", Status: 200,
		Request:  []byte(`{"requestId": "agent/S1/1/x/3", "request": {"contents": []}}`),
		Response: []byte("data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"hi\"}]}}]}\n\n"),
	}); err != nil {
		t.Fatal(err)
	}

	anon := e.client("203.0.113.5")
	if r := anon.do("GET", "/admin/api/requests/1/upstream", nil, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", r.status)
	}
	if r := anon.do("GET", "/admin/api/upstream-calls/1", nil, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous call = %d", r.status)
	}

	c := e.client("203.0.113.6")
	c.login(testPassword)
	r := c.do("GET", "/admin/api/requests", nil, nil)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.raw), &rows); err != nil || len(rows) != 1 || rows[0]["upstream"] != float64(1) {
		t.Fatalf("requests = %s, %v", r.raw, err)
	}

	r = c.do("GET", "/admin/api/requests/1/upstream", nil, nil)
	var infos []map[string]any
	if err := json.Unmarshal([]byte(r.raw), &infos); err != nil || len(infos) != 1 || infos[0]["seq"] != float64(3) || infos[0]["session"] != "S1" {
		t.Fatalf("list = %s, %v", r.raw, err)
	}

	r = c.do("GET", fmt.Sprintf("/admin/api/upstream-calls/%v", infos[0]["id"]), nil, nil)
	response, _ := r.body["response"].(map[string]any)
	parts, _ := response["parts"].([]any)
	if r.status != http.StatusOK || r.body["request"] == nil || len(parts) != 1 {
		t.Fatalf("call = %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/admin/api/upstream-calls/999", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("missing call = %d", r.status)
	}
	if r := c.do("GET", "/admin/api/upstream-calls/x", nil, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad id = %d", r.status)
	}
}

func TestTurningCaptureOffDeletesTheCalls(t *testing.T) {
	e := newEnv(t)
	if err := SetPassword(context.Background(), e.st, testPassword); err != nil {
		t.Fatal(err)
	}
	c := e.client("203.0.113.7")
	c.login(testPassword)

	put := func(mutate func(map[string]any)) reply {
		settings := c.do("GET", "/admin/api/settings", nil, nil).body
		delete(settings, "defaults")
		mutate(settings)
		return c.do("PUT", "/admin/api/settings", settings, nil)
	}
	if r := put(func(s map[string]any) { s["capture_upstream"] = true }); r.status != http.StatusOK || r.body["capture_upstream"] != true || r.body["capture_response_mib"] != float64(4) {
		t.Fatalf("turn on = %d %s", r.status, r.raw)
	}
	if r := put(func(s map[string]any) { s["capture_response_mib"] = 0 }); r.status != http.StatusBadRequest {
		t.Fatalf("limit 0 = %d", r.status)
	}

	ctx := context.Background()
	if err := e.st.AddUpstreamCall(ctx, store.UpstreamCall{At: time.Now(), CallToken: "t", Request: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if r := put(func(s map[string]any) { s["capture_upstream"] = false }); r.status != http.StatusOK {
		t.Fatalf("turn off = %d %s", r.status, r.raw)
	}
	if _, err := e.st.UpstreamCall(ctx, 1); err == nil {
		t.Fatal("captured calls survived turning capture off")
	}
}
