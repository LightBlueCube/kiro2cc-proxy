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

func TestFlagsSurviveRestartUntilCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	ctx := context.Background()
	st, f := open(t, path)

	now := time.Now()
	for fingerprint, until := range map[string]time.Time{"live": now.Add(time.Hour), "over": now.Add(-time.Hour)} {
		if err := f.Flag(ctx, gateway.FlaggedPrompt{Fingerprint: fingerprint, Text: "text " + fingerprint, At: now.Add(-2 * time.Hour), Until: until, KeyName: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.FlaggedUntil("live"); got.IsZero() || !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("live until = %v", got)
	}
	if !f.FlaggedUntil("unknown").IsZero() {
		t.Fatal("unknown prompt flagged")
	}
	st.Close()

	_, f = open(t, path)
	if f.FlaggedUntil("live").IsZero() || !time.Now().Before(f.FlaggedUntil("live")) {
		t.Fatal("running flag lost over a restart")
	}
	if !f.FlaggedUntil("over").IsZero() {
		t.Fatal("ended flag loaded")
	}

	if err := f.Cancel(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if time.Now().Before(f.FlaggedUntil("live")) {
		t.Fatal("flag still running after cancel")
	}
	if err := f.Cancel(ctx, "live"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second cancel: %v", err)
	}
	list, err := f.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if text, err := f.Text(ctx, "live"); err != nil || text != "text live" {
		t.Fatalf("text = %q, %v", text, err)
	}

	_, f = open(t, path)
	if !f.FlaggedUntil("live").IsZero() {
		t.Fatal("cancelled flag loaded after a restart")
	}
}
