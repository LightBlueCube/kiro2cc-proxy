package usage

import (
	"context"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agyrelay/internal/conv"
	"agyrelay/internal/gateway"
	"agyrelay/internal/store"
)

func TestPrices(t *testing.T) {
	if p, ok := priceOf(DefaultPrices, "gemini-4-argon"); !ok || p.Input != 2 {
		t.Fatalf("argon = %+v %v", p, ok)
	}
	if _, ok := priceOf(DefaultPrices, "gpt-oss-120b-medium"); ok {
		t.Fatal("a model without a price has one")
	}
	if err := ValidatePrices(append(slices.Clone(DefaultPrices), Price{Model: "gemini-4-argon"})); err == nil {
		t.Fatal("a second price for a model passed")
	}
	// 1M prompt tokens of which 400k cached, 100k output.
	got := Price{Input: 2, Cached: 0.1, Output: 10}.cost(1_000_000, 400_000, 100_000)
	if want := 0.6*2 + 0.4*0.1 + 0.1*10; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
	// A cache-write price replaces the input price for uncached tokens.
	got = Price{Input: 2, Cached: 0.1, CacheWrite: 2.5, Output: 10}.cost(1_000_000, 400_000, 100_000)
	if want := 0.6*2.5 + 0.4*0.1 + 0.1*10; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost with cache writes = %v, want %v", got, want)
	}
}

func TestRecordAndAttribute(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	rec := NewRecorder(st, []Price{{Model: "m", Input: 1, Cached: 0, Output: 1}}, log)

	t0 := time.Now().Add(-time.Hour)
	attributor := NewAttributor(st, log)
	sample := func(at time.Time, remaining float64, busy bool) {
		t.Helper()
		if err := st.AddQuotaSamples(ctx, []store.QuotaSample{{Account: "a", At: at, Bucket: "gemini-weekly", Group: "Gemini",
			Window: "weekly", Remaining: remaining, ResetAt: t0.Add(7 * 24 * time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		attributor.Attribute(ctx, "a", at, busy)
	}
	sample(t0, 0.9, false)
	// Two requests, the second costing three times the first.
	for i, tokens := range []int64{1000, 3000} {
		rec.Record(gateway.Record{
			At: t0.Add(time.Duration(i+1) * time.Minute), KeyHash: "k", KeyName: "alice", Account: "a", Model: "m", QuotaGroup: "gemini",
			Usage: &conv.Usage{PromptTokens: tokens},
		})
	}
	rec.Record(gateway.Record{At: t0.Add(3 * time.Minute), KeyHash: "k", KeyName: "alice", Account: "a", Model: "m", QuotaGroup: "gemini",
		Err: &conv.Error{Kind: conv.ErrQuota, Message: "limit"}})
	sample(t0.Add(5*time.Minute), 0.86, false)

	rows, err := st.Requests(ctx, store.RequestFilter{}, 0, 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	failed, second, first := rows[0], rows[1], rows[2]
	if failed.Status != "quota" || failed.QuotaUsed != nil || failed.CostUSD != nil {
		t.Fatalf("failed = %+v", failed)
	}
	if *first.CostUSD != 0.001 || math.Abs(*first.QuotaUsed-0.01) > 1e-9 || math.Abs(*second.QuotaUsed-0.03) > 1e-9 {
		t.Fatalf("first = %+v / %v, second = %v", first, *first.QuotaUsed, *second.QuotaUsed)
	}

	sums, err := st.Summarize(ctx, store.RequestFilter{}, store.ByKey, 0)
	if err != nil || len(sums) != 1 || sums[0].Group != "k" || sums[0].KeyName != "alice" || sums[0].Requests != 3 || sums[0].Errors != 1 || math.Abs(sums[0].QuotaUsed-0.04) > 1e-9 {
		t.Fatalf("summary = %+v, %v", sums, err)
	}

	record := func(at time.Duration, key string) {
		rec.Record(gateway.Record{At: t0.Add(at), KeyHash: key, Account: "a", Model: "m", QuotaGroup: "gemini", Usage: &conv.Usage{PromptTokens: 1000}})
	}
	// A sample during a running turn carries its drop to the next idle one.
	record(6*time.Minute, "k")
	sample(t0.Add(10*time.Minute), 0.85, true)
	sample(t0.Add(15*time.Minute), 0.84, false)
	// A sample right after a request waits for the quota to catch up.
	record(19*time.Minute, "k")
	sample(t0.Add(20*time.Minute), 0.84, false)
	sample(t0.Add(25*time.Minute), 0.83, false)
	// A drop with no request is use outside agyrelay.
	sample(t0.Add(30*time.Minute), 0.80, false)
	record(31*time.Minute, "k2")
	sample(t0.Add(40*time.Minute), 0.79, false)
	// Not sampled yet: estimated.
	record(45*time.Minute, "k")

	rows, err = st.Requests(ctx, store.RequestFilter{}, 0, 10)
	if err != nil || len(rows) != 7 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	want := []float64{-1, 0.01, 0.01, 0.02} // newest first; -1 = unattributed
	for i, w := range want {
		got := rows[i].QuotaUsed
		if (w < 0) != (got == nil) || (got != nil && math.Abs(*got-w) > 1e-9) {
			t.Fatalf("row %d (%s) quota = %v, want %v", i, rows[i].At.Sub(t0), got, w)
		}
	}

	uses, err := NewBudget(st).Uses(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	// 0.08 of quota for $0.007 of attributed requests; the pending one costs $0.001.
	k, k2 := uses["k"]["gemini"], uses["k2"]["gemini"]
	if math.Abs(k.Attributed-0.07) > 1e-9 || math.Abs(k.Estimated-0.08/7) > 1e-9 || math.Abs(k2.Total()-0.01) > 1e-9 {
		t.Fatalf("uses = %+v", uses)
	}
	if used, err := NewBudget(st).Used(ctx, "k", "claude-gpt"); err != nil || used != 0 {
		t.Fatalf("claude-gpt used = %v, %v", used, err)
	}

	// Use never resets: pruning every request keeps the attributed part.
	if err := st.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if used, err := NewBudget(st).Used(ctx, "k", "gemini"); err != nil || math.Abs(used-0.07) > 1e-9 {
		t.Fatalf("after pruning, used = %v, %v", used, err)
	}
}

// Cloud Code answers some quota reads with an older, higher value under
// the same reset time; only a later reset time refills the bucket.
func TestStaleQuotaSamples(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	rec := NewRecorder(st, []Price{{Model: "m", Input: 1, Output: 1}}, log)
	attributor := NewAttributor(st, log)

	t0 := time.Now().Add(-time.Hour)
	week := t0.Add(7 * 24 * time.Hour)
	sample := func(at time.Duration, remaining float64, resetAt time.Time) {
		t.Helper()
		if err := st.AddQuotaSamples(ctx, []store.QuotaSample{{Account: "a", At: t0.Add(at), Bucket: "gemini-weekly", Group: "Gemini",
			Window: "weekly", Remaining: remaining, ResetAt: resetAt}}); err != nil {
			t.Fatal(err)
		}
		attributor.Attribute(ctx, "a", t0.Add(at), false)
	}
	record := func(at time.Duration) {
		rec.Record(gateway.Record{At: t0.Add(at), KeyHash: "k", Account: "a", Model: "m", QuotaGroup: "gemini", Usage: &conv.Usage{PromptTokens: 1000}})
	}

	sample(0, 0.90, week)
	record(time.Minute)
	sample(5*time.Minute, 0.86, week) // 0.04
	record(6 * time.Minute)
	sample(10*time.Minute, 0.88, week) // stale: carried
	sample(15*time.Minute, 0.85, week) // 0.01, not 0.88 - 0.85
	// A later reset time with more quota left is a refill.
	sample(20*time.Minute, 1.0, week.Add(7*24*time.Hour))
	record(21 * time.Minute)
	sample(25*time.Minute, 0.98, week.Add(7*24*time.Hour)) // 0.02

	rows, err := st.Requests(ctx, store.RequestFilter{}, 0, 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	for i, want := range []float64{0.02, 0.01, 0.04} { // newest first
		if got := rows[i].QuotaUsed; got == nil || math.Abs(*got-want) > 1e-9 {
			t.Fatalf("row %d quota = %v, want %v", i, got, want)
		}
	}
}
