package usage

import (
	"context"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"agyrelay/internal/conv"
	"agyrelay/internal/gateway"
	"agyrelay/internal/store"
)

func TestPriceAt(t *testing.T) {
	before := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	after := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if p, ok := priceAt(DefaultPrices, "gemini-4-argon", before); !ok || p.Input != 2 {
		t.Fatalf("before = %+v %v", p, ok)
	}
	if p, ok := priceAt(DefaultPrices, "gemini-4-argon", after); !ok || p.Input != 4 {
		t.Fatalf("after = %+v %v", p, ok)
	}
	if _, ok := priceAt(DefaultPrices, "gpt-oss-120b", after); ok {
		t.Fatal("gpt-oss-120b has a price")
	}
	// 1M prompt tokens of which 400k cached, 100k output.
	got := Price{Input: 2, Cached: 0.1, Output: 10}.cost(1_000_000, 400_000, 100_000)
	if want := 0.6*2 + 0.4*0.1 + 0.1*10; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
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
	sample := func(at time.Time, remaining float64) {
		if err := st.AddQuotaSamples(ctx, []store.QuotaSample{{Account: "a", At: at, Bucket: "gemini-weekly", Group: "Gemini", Window: "weekly", Remaining: remaining}}); err != nil {
			t.Fatal(err)
		}
	}
	sample(t0, 0.9)
	// Two requests, the second costing three times the first.
	for i, tokens := range []int64{1000, 3000} {
		rec.Record(gateway.Record{
			At: t0.Add(time.Duration(i+1) * time.Minute), Key: "k", Account: "a", Model: "m", QuotaGroup: "gemini",
			Usage: &conv.Usage{PromptTokens: tokens},
		})
	}
	rec.Record(gateway.Record{At: t0.Add(3 * time.Minute), Key: "k", Account: "a", Model: "m", QuotaGroup: "gemini",
		Err: &conv.Error{Kind: conv.ErrQuota, Message: "limit"}})
	sample(t0.Add(5*time.Minute), 0.86)
	NewAttributor(st, log).Attribute(ctx, "a", t0.Add(5*time.Minute))

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
	if err != nil || len(sums) != 1 || sums[0].Requests != 3 || sums[0].Errors != 1 || math.Abs(sums[0].QuotaUsed-0.04) > 1e-9 {
		t.Fatalf("summary = %+v, %v", sums, err)
	}
}
