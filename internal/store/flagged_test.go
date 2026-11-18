package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFlaggedPrompts(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	first := time.Now().Truncate(time.Millisecond)
	if err := st.FlagPrompt(ctx, FlaggedPrompt{Fingerprint: "f1", Text: "You are opencode",
		Request: []byte(`{"first":true}`), Response: []byte(`{"error":"first"}`),
		LastAt: first, Until: first.Add(24 * time.Hour),
		KeyHash: "h1", KeyName: "alice", Account: "a1", Session: "S1", Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.FlagPrompt(ctx, FlaggedPrompt{Fingerprint: "f2", Text: "other", LastAt: first.Add(time.Minute), Until: first.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// Refused again later by another key: the text and the first time
	// stay, and the latest refusal's details and upstream call replace the
	// earlier ones.
	again := first.Add(2 * time.Hour)
	latestReq := bytes.Repeat([]byte(`{"request":{"contents":[]}}`), 200)
	latestResp := []byte(`{"error":{"code":429,"message":"Resource has been exhausted"}}`)
	if err := st.FlagPrompt(ctx, FlaggedPrompt{Fingerprint: "f1", Text: "ignored",
		Request: latestReq, Response: latestResp,
		LastAt: again, Until: again.Add(24 * time.Hour),
		KeyHash: "h2", KeyName: "bob", Account: "a2", Session: "S2", Model: "m2"}); err != nil {
		t.Fatal(err)
	}
	list, err := st.FlaggedPrompts(ctx)
	if err != nil || len(list) != 2 || list[0].Fingerprint != "f1" || list[1].Fingerprint != "f2" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	want := FlaggedPrompt{Fingerprint: "f1", FirstAt: first, LastAt: again, Until: again.Add(24 * time.Hour), Hits: 2,
		KeyHash: "h2", KeyName: "bob", Account: "a2", AccountName: "a2", Session: "S2", Model: "m2"}
	got := list[0]
	sameTimes := got.FirstAt.Equal(want.FirstAt) && got.LastAt.Equal(want.LastAt) && got.Until.Equal(want.Until)
	got.FirstAt, got.LastAt, got.Until = time.Time{}, time.Time{}, time.Time{}
	want.FirstAt, want.LastAt, want.Until = time.Time{}, time.Time{}, time.Time{}
	if !sameTimes || !reflect.DeepEqual(got, want) {
		t.Fatalf("f1 = %+v (times equal: %v), want %+v", got, sameTimes, want)
	}
	detail, err := st.FlaggedPrompt(ctx, "f1")
	if err != nil || detail.Text != "You are opencode" || detail.Session != "S2" ||
		!bytes.Equal(detail.Request, latestReq) || !bytes.Equal(detail.Response, latestResp) {
		t.Fatalf("detail f1 = %+v, %v", detail, err)
	}
	// A row without a captured upstream call (e.g. a legacy row) unpacks nil slices.
	legacy, err := st.FlaggedPrompt(ctx, "f2")
	if err != nil || legacy.Text != "other" || legacy.Request != nil || legacy.Response != nil {
		t.Fatalf("detail f2 = %+v, %v", legacy, err)
	}
	if _, err := st.FlaggedPrompt(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown prompt: %v", err)
	}

	// At first+2h only f1 is still flagged.
	active, err := st.ActiveFlaggedPrompts(ctx, first.Add(90*time.Minute))
	if err != nil || len(active) != 1 || active[0].Fingerprint != "f1" {
		t.Fatalf("active = %+v, %v", active, err)
	}

	// Cancelling ends the flag and keeps the row; a second cancel finds none.
	now := first.Add(3 * time.Hour)
	if err := st.CancelFlaggedPrompt(ctx, "f1", now); err != nil {
		t.Fatal(err)
	}
	if err := st.CancelFlaggedPrompt(ctx, "f1", now.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second cancel: %v", err)
	}
	if err := st.CancelFlaggedPrompt(ctx, "f2", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel of an expired flag: %v", err)
	}
	if active, err := st.ActiveFlaggedPrompts(ctx, now.Add(time.Second)); err != nil || len(active) != 0 {
		t.Fatalf("active after cancel = %+v, %v", active, err)
	}

	// Request retention does not touch them.
	if err := st.Prune(ctx, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if list, err := st.FlaggedPrompts(ctx); err != nil || len(list) != 2 {
		t.Fatalf("list after prune = %+v, %v", list, err)
	}
}

func fingerprints(t *testing.T, st *Store) []string {
	t.Helper()
	list, err := st.FlaggedPrompts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range list {
		out = append(out, p.Fingerprint)
	}
	return out
}

func TestPinnedFlaggedPromptsStayAndRestartRetentionWhenReleased(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	day := 24 * time.Hour
	start := time.Now().Truncate(time.Millisecond)
	for _, fp := range []string{"old", "pinned", "unpinned", "cancelled"} {
		if err := st.FlagPrompt(ctx, FlaggedPrompt{Fingerprint: fp, Text: fp, LastAt: start, Until: start.Add(day)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, fp := range []string{"pinned", "unpinned", "cancelled"} {
		if err := st.PinFlaggedPrompt(ctx, fp); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PinFlaggedPrompt(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pin of an unknown prompt: %v", err)
	}

	// Pinned stays active after its flag ran out.
	later := start.Add(40 * day)
	active, err := st.ActiveFlaggedPrompts(ctx, later)
	if err != nil || len(active) != 3 || !active[0].Pinned {
		t.Fatalf("active = %+v, %v", active, err)
	}

	// Releasing restarts the retention from then, not from the last
	// refusal, which is 40 days back.
	if err := st.UnpinFlaggedPrompt(ctx, "unpinned", later); err != nil {
		t.Fatal(err)
	}
	if err := st.UnpinFlaggedPrompt(ctx, "unpinned", later); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second unpin: %v", err)
	}
	if err := st.CancelFlaggedPrompt(ctx, "cancelled", later); err != nil {
		t.Fatal(err)
	}
	if err := st.CancelFlaggedPrompt(ctx, "cancelled", later); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second cancel: %v", err)
	}
	if err := st.PruneFlaggedPrompts(ctx, later.Add(-30*day)); err != nil {
		t.Fatal(err)
	}
	if got := fingerprints(t, st); len(got) != 3 || got[0] == "old" || got[1] == "old" || got[2] == "old" {
		t.Fatalf("after the first prune: %v, want old gone", got)
	}
	if err := st.PruneFlaggedPrompts(ctx, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := fingerprints(t, st); len(got) != 1 || got[0] != "pinned" {
		t.Fatalf("after the second prune: %v, want only pinned", got)
	}
}
