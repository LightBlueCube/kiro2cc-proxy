package promptflags

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"agyrelay/internal/gateway"
	"agyrelay/internal/store"
)

func open(t *testing.T, path string) (*store.Store, *Flags) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f, err := New(context.Background(), st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return st, f
}

// until is when the prompt's flag ends, ignoring pins.
func until(f *Flags, fingerprint string) time.Time {
	u, _ := f.Flagged(fingerprint)
	return u
}

func refused(f *Flags, fingerprint string) bool {
	u, pinned := f.Flagged(fingerprint)
	return pinned || time.Now().Before(u)
}

func TestFlagsSurviveRestartUntilCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	ctx := context.Background()
	st, f := open(t, path)

	now := time.Now()
	for fingerprint, until := range map[string]time.Time{"live": now.Add(time.Hour), "over": now.Add(-time.Hour)} {
		if err := f.Flag(ctx, gateway.FlaggedPrompt{
			Fingerprint: fingerprint, Text: "text " + fingerprint,
			Request: []byte(`{"fp":"` + fingerprint + `"}`), Response: []byte("429"),
			At: now.Add(-2 * time.Hour), Until: until, KeyName: "alice", Session: "S1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := until(f, "live"); got.IsZero() || !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("live until = %v", got)
	}
	if !until(f, "unknown").IsZero() {
		t.Fatal("unknown prompt flagged")
	}
	st.Close()

	_, f = open(t, path)
	if !refused(f, "live") {
		t.Fatal("running flag lost over a restart")
	}
	if !until(f, "over").IsZero() {
		t.Fatal("ended flag loaded")
	}

	if err := f.Cancel(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if refused(f, "live") {
		t.Fatal("flag still running after cancel")
	}
	if err := f.Cancel(ctx, "live"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second cancel: %v", err)
	}
	list, err := f.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if got, err := f.Get(ctx, "live"); err != nil || got.Text != "text live" || string(got.Request) != `{"fp":"live"}` || string(got.Response) != "429" || got.Session != "S1" {
		t.Fatalf("get = %+v, %v", got, err)
	}

	_, f = open(t, path)
	if refused(f, "live") {
		t.Fatal("cancelled flag loaded after a restart")
	}
}

func TestPinnedPromptStaysRefusedUntilUnpinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	ctx := context.Background()
	_, f := open(t, path)

	now := time.Now()
	for fingerprint, until := range map[string]time.Time{"running": now.Add(time.Hour), "over": now.Add(-time.Hour)} {
		if err := f.Flag(ctx, gateway.FlaggedPrompt{Fingerprint: fingerprint, Text: fingerprint, At: now.Add(-2 * time.Hour), Until: until}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Pin(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pin of an unknown prompt: %v", err)
	}
	if err := f.Unpin(ctx, "over"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unpin of a prompt that is not pinned: %v", err)
	}
	for _, fingerprint := range []string{"running", "over"} {
		if err := f.Pin(ctx, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	if !refused(f, "over") {
		t.Fatal("pinned prompt not refused after its flag ended")
	}

	// Another refusal of a pinned prompt keeps the pin.
	if err := f.Flag(ctx, gateway.FlaggedPrompt{Fingerprint: "over", At: now, Until: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, pinned := f.Flagged("over"); !pinned {
		t.Fatal("pin lost on a new refusal")
	}

	// Pins and the flags behind them survive a restart.
	_, f = open(t, path)
	if _, pinned := f.Flagged("over"); !pinned {
		t.Fatal("pin lost over a restart")
	}

	// Unpinning leaves a flag that is still running, and drops one that ended.
	if err := f.Unpin(ctx, "running"); err != nil {
		t.Fatal(err)
	}
	if _, pinned := f.Flagged("running"); pinned || !refused(f, "running") {
		t.Fatal("unpinned prompt with a running flag should be refused until it ends")
	}
	if err := f.Cancel(ctx, "over"); err != nil {
		t.Fatal(err)
	}
	if refused(f, "over") {
		t.Fatal("cancel did not lift a pinned prompt")
	}
	if err := f.Unpin(ctx, "over"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unpin after cancel: %v", err)
	}
	if err := f.Pin(ctx, "over"); err != nil {
		t.Fatal(err)
	}
	if err := f.Unpin(ctx, "over"); err != nil {
		t.Fatal(err)
	}
	if refused(f, "over") {
		t.Fatal("unpinned prompt with no running flag still refused")
	}

	// Deleting refuses a running or pinned prompt, and removes an ended one.
	if err := f.Delete(ctx, "running"); !errors.Is(err, store.ErrFlagActive) {
		t.Fatalf("delete while running = %v, want ErrFlagActive", err)
	}
	if err := f.Pin(ctx, "over"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "over"); !errors.Is(err, store.ErrFlagActive) {
		t.Fatalf("delete while pinned = %v, want ErrFlagActive", err)
	}
	if err := f.Unpin(ctx, "over"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "over"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "over"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := f.Get(ctx, "over"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted prompt still readable: %v", err)
	}
}

func TestDisabledFlagsRefuseNothing(t *testing.T) {
	ctx := context.Background()
	_, f := open(t, filepath.Join(t.TempDir(), "db"))
	if err := f.Flag(ctx, gateway.FlaggedPrompt{Fingerprint: "p", Text: "p", At: time.Now(), Until: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := f.Pin(ctx, "p"); err != nil {
		t.Fatal(err)
	}

	f.SetEnabled(false)
	if refused(f, "p") || f.Enabled() {
		t.Fatal("flag refused while disabled")
	}
	f.SetEnabled(true)
	if !refused(f, "p") {
		t.Fatal("flag lost while disabled")
	}
}
