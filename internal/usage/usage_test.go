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
	if p, ok := priceOf(DefaultPrices, "gemini-4-argon"); !ok || p.Input != 4 {
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
	var settled []string
	rec := NewRecorder(st, []Price{{Model: "m", Input: 1, Cached: 0, Output: 1}}, func(account string) { settled = append(settled, account) }, log)

	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond) // stored times keep milliseconds
	attributor := NewAttributor(st, log)
	sample := func(at time.Time, remaining float64, oldestRun time.Time) {
		t.Helper()
		if err := st.AddQuotaSamples(ctx, []store.QuotaSample{{Account: "a", At: at, Bucket: "gemini-weekly", Group: "Gemini",
			Window: "weekly", Remaining: remaining, ResetAt: t0.Add(7 * 24 * time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		attributor.Attribute(ctx, "a", at, oldestRun)
	}
	sample(t0, 0.9, time.Time{})
	// Two requests, the second costing three times the first.
	for i, tokens := range []int64{1000, 3000} {
		rec.Record(gateway.Record{
			At: t0.Add(time.Duration(i+1) * time.Minute), KeyHash: "k", KeyName: "alice", Account: "a", Model: "m", QuotaGroup: "gemini",
			Usage: &conv.Usage{PromptTokens: tokens},
		})
	}
	rec.Record(gateway.Record{At: t0.Add(3 * time.Minute), KeyHash: "k", KeyName: "alice", Account: "a", Model: "m", QuotaGroup: "gemini",
		Err: &conv.Error{Kind: conv.ErrQuota, Message: "limit"}})
	// Requests that used tokens ask for a settling sample; the failed one does not.
	if !slices.Equal(settled, []string{"a", "a"}) {
		t.Fatalf("settle requests = %v", settled)
	}
	sample(t0.Add(2*time.Minute+SettleTime), 0.86, time.Time{})

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
	if err != nil || len(sums) != 1 || sums[0].Group != "k" || sums[0].KeyName != "alice" || sums[0].Requests != 3 || sums[0].Errors != 1 {
		t.Fatalf("summary = %+v, %v", sums, err)
	}

	record := func(at time.Duration, key string) {
		rec.Record(gateway.Record{At: t0.Add(at), KeyHash: key, Account: "a", Model: "m", QuotaGroup: "gemini", Usage: &conv.Usage{PromptTokens: 1000}})
	}
	// A sample while a turn that began before its cutoff runs carries its
	// drop to the next sample without one.
	record(10*time.Minute, "k")
	sample(t0.Add(10*time.Minute+SettleTime), 0.85, t0.Add(10*time.Minute)) // began at the cutoff
	sample(t0.Add(15*time.Minute+SettleTime), 0.84, time.Time{})
	// A sample before the quota caught up with a request waits.
	record(30*time.Minute, "k")
	sample(t0.Add(31*time.Minute), 0.835, time.Time{}) // part of the use shows
	sample(t0.Add(30*time.Minute+SettleTime), 0.83, time.Time{})
	// A drop with no request is use outside agyrelay.
	sample(t0.Add(45*time.Minute), 0.80, time.Time{})
	record(46*time.Minute, "k2")
	sample(t0.Add(46*time.Minute+SettleTime), 0.79, time.Time{})
	// Not sampled yet.
	record(60*time.Minute, "k")

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

	report, err := Quota(ctx, st, store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]float64{}
	for _, u := range report.Uses {
		used[u.KeyHash+" "+u.Account+" "+u.Group] = u.Used
	}
	if len(used) != 2 || math.Abs(used["k a gemini"]-0.07) > 1e-9 || math.Abs(used["k2 a gemini"]-0.01) > 1e-9 ||
		!slices.Equal(report.Accounts["gemini"], []string{"a"}) {
		t.Fatalf("quota report = %+v", report)
	}

	// Spend is the cost of every priced request, sampled or not: five
	// requests of 1000 or 3000 tokens at $1 per million.
	budget := NewBudget(st, rec)
	spent, err := budget.Spent(ctx, "k")
	if err != nil || math.Abs(spent["gemini"]-0.007) > 1e-9 || len(spent) != 1 {
		t.Fatalf("spent = %v, %v", spent, err)
	}
	if !budget.Priced("m") || budget.Priced("other") {
		t.Fatal("priced models are wrong")
	}

	// Spend never resets: pruning every request keeps it.
	if err := st.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	spends, err := budget.Spends(ctx, "")
	if err != nil || math.Abs(spends["k"]["gemini"]-0.007) > 1e-9 || math.Abs(spends["k2"]["gemini"]-0.001) > 1e-9 {
		t.Fatalf("after pruning, spends = %v, %v", spends, err)
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
	rec := NewRecorder(st, []Price{{Model: "m", Input: 1, Output: 1}}, nil, log)
	attributor := NewAttributor(st, log)

	t0 := time.Now().Add(-time.Hour)
	week := t0.Add(7 * 24 * time.Hour)
	sample := func(at time.Duration, remaining float64, resetAt time.Time) {
		t.Helper()
		if err := st.AddQuotaSamples(ctx, []store.QuotaSample{{Account: "a", At: t0.Add(at), Bucket: "gemini-weekly", Group: "Gemini",
			Window: "weekly", Remaining: remaining, ResetAt: resetAt}}); err != nil {
			t.Fatal(err)
		}
		attributor.Attribute(ctx, "a", t0.Add(at), time.Time{})
	}
	record := func(at time.Duration) {
		rec.Record(gateway.Record{At: t0.Add(at), KeyHash: "k", Account: "a", Model: "m", QuotaGroup: "gemini", Usage: &conv.Usage{PromptTokens: 1000}})
	}

	sample(0, 0.90, week)
	record(time.Minute)
	sample(time.Minute+SettleTime, 0.86, week) // 0.04
	record(10 * time.Minute)
	sample(10*time.Minute+SettleTime, 0.88, week)         // stale: carried
	sample(11*time.Minute+SettleTime, 0.85, week)         // 0.01, not 0.88 - 0.85
	sample(30*time.Minute, 1.0, week.Add(7*24*time.Hour)) // a later reset time with more quota left is a refill
	record(31 * time.Minute)
	sample(31*time.Minute+SettleTime, 0.98, week.Add(7*24*time.Hour)) // 0.02

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

// Under continuous traffic, with a turn always running, each sample settles
// the requests that ended SettleTime before it, so the use is attributed
// while the traffic goes on.
func TestSettlesUnderContinuousTraffic(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	rec := NewRecorder(st, []Price{{Model: "m", Input: 1, Output: 1}}, nil, log)
	attributor := NewAttributor(st, log)

	const (
		requests = 40
		lag      = SettleTime // the quota shows a request's use this long after it ended
		perUse   = 0.01
	)
	t0 := time.Now().Add(-2 * time.Hour)
	week := t0.Add(7 * 24 * time.Hour)
	remaining := func(at time.Duration) float64 {
		shown := max(0, int((at-lag)/time.Minute)) // one request ended each minute from the first
		return 0.9 - perUse*float64(min(shown, requests))
	}
	sample := func(at time.Duration, oldestRun time.Time) {
		t.Helper()
		if err := st.AddQuotaSamples(ctx, []store.QuotaSample{{Account: "a", At: t0.Add(at), Bucket: "gemini-weekly", Group: "Gemini",
			Window: "weekly", Remaining: remaining(at), ResetAt: week}}); err != nil {
			t.Fatal(err)
		}
		attributor.Attribute(ctx, "a", t0.Add(at), oldestRun)
	}

	sample(0, time.Time{})
	for minute := 1; minute <= requests+10; minute++ {
		at := time.Duration(minute) * time.Minute
		if minute <= requests {
			rec.Record(gateway.Record{At: t0.Add(at), KeyHash: "k", Account: "a", Model: "m", QuotaGroup: "gemini", Usage: &conv.Usage{PromptTokens: 1000}})
		}
		// A turn that began a minute ago is always running.
		sample(at, t0.Add(at-time.Minute))
	}

	rows, err := st.Requests(ctx, store.RequestFilter{}, 0, requests+10)
	if err != nil || len(rows) != requests {
		t.Fatalf("rows = %d, %v", len(rows), err)
	}
	total := 0.0
	for _, row := range rows {
		if row.QuotaUsed == nil {
			t.Fatalf("request at %v is not settled", row.At.Sub(t0))
		}
		if math.Abs(*row.QuotaUsed-perUse) > 1e-9 {
			t.Fatalf("request at %v used %v, want %v", row.At.Sub(t0), *row.QuotaUsed, perUse)
		}
		total += *row.QuotaUsed
	}
	if want := 0.9 - remaining(time.Duration(requests+10)*time.Minute); math.Abs(total-want) > 1e-9 {
		t.Fatalf("attributed %v of a %v drop", total, want)
	}
}

// A request stored after a sample passed its start is settled by a later
// sample; requests that cannot be settled get a reason instead.
func TestLateRecordsAndSkips(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	rec := NewRecorder(st, []Price{{Model: "m", Input: 1, Output: 1}}, nil, log)
	attributor := NewAttributor(st, log)

	t0 := time.Now().Add(-time.Hour)
	week := t0.Add(7 * 24 * time.Hour)
	sample := func(at time.Duration, remaining float64, resetAt time.Time, more ...store.QuotaSample) {
		t.Helper()
		samples := []store.QuotaSample{{Account: "a", At: t0.Add(at), Bucket: "gemini-weekly", Group: "Gemini",
			Window: "weekly", Remaining: remaining, ResetAt: resetAt}}
		if err := st.AddQuotaSamples(ctx, append(samples, more...)); err != nil {
			t.Fatal(err)
		}
		attributor.Attribute(ctx, "a", t0.Add(at), time.Time{})
	}
	record := func(at time.Duration, group string) {
		rec.Record(gateway.Record{At: t0.Add(at), Duration: 30 * time.Second, KeyHash: "k", Account: "a", Model: "m", QuotaGroup: group,
			Usage: &conv.Usage{PromptTokens: 1000}})
	}

	record(-10*time.Minute, "gemini") // before the first sample
	sample(0, 0.90, week, store.QuotaSample{Account: "a", At: t0, Bucket: "3p-weekly", Group: "Claude", Window: "weekly", Remaining: 1, ResetAt: week})
	// A sample while a request runs, before its record is stored, finds
	// nothing to settle and moves the cursor past the request's start.
	sample(time.Minute+5*time.Second, 0.90, week)
	record(time.Minute, "gemini")
	sample(90*time.Second+SettleTime, 0.88, week)
	// The claude-gpt bucket was in the first sample only: the plan has
	// none now.
	record(20*time.Minute, "claude-gpt")
	// A reset before a request's use showed.
	record(30*time.Minute, "gemini")
	sample(31*time.Minute, 1.0, week.Add(7*24*time.Hour))

	rows, err := st.Requests(ctx, store.RequestFilter{}, 0, 10)
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	reset, noBucket, late, early := rows[0], rows[1], rows[2], rows[3]
	if late.QuotaUsed == nil || math.Abs(*late.QuotaUsed-0.02) > 1e-9 || late.QuotaSkip != "" {
		t.Fatalf("late record = %v %q", late.QuotaUsed, late.QuotaSkip)
	}
	for _, c := range []struct {
		row  store.Request
		want store.QuotaSkip
	}{{early, store.SkipBeforeSampling}, {noBucket, store.SkipNoBucket}, {reset, store.SkipReset}} {
		if c.row.QuotaUsed != nil || c.row.QuotaSkip != c.want {
			t.Fatalf("request at %s: quota %v, skip %q, want %q", c.row.At.Sub(t0), c.row.QuotaUsed, c.row.QuotaSkip, c.want)
		}
	}
}

func TestRecordErrorStatuses(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec := NewRecorder(st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	cases := []struct {
		kind conv.ErrorKind
		want string
	}{
		{conv.ErrKeyLimit, "key_limit"},
		{conv.ErrKeyBusy, "key_busy"},
		{conv.ErrCancelled, "cancelled"},
		{conv.ErrQuota, "quota"},
	}
	for i, tc := range cases {
		rec.Record(gateway.Record{
			At: t0.Add(time.Duration(i) * time.Second), KeyHash: "k", KeyName: "alice", Model: "m", QuotaGroup: "gemini",
			Err: &conv.Error{Kind: tc.kind, HTTPStatus: 403, Message: tc.want},
		})
	}
	rows, err := st.Requests(ctx, store.RequestFilter{}, 0, 10)
	if err != nil || len(rows) != len(cases) {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	for i, tc := range cases {
		row := rows[len(cases)-1-i]
		if row.Status != tc.want {
			t.Errorf("kind %v -> status %q, want %q", tc.kind, row.Status, tc.want)
		}
	}
}
