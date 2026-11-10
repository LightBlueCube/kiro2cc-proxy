package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agyrelay/internal/conv"
	"agyrelay/internal/gateway"
	"agyrelay/internal/store"
)

const recordTimeout = 10 * time.Second

// Recorder stores the gateway's request records with their list-price
// cost.
type Recorder struct {
	store *store.Store
	log   *slog.Logger

	mu     sync.Mutex
	prices []Price
}

func NewRecorder(st *store.Store, prices []Price, log *slog.Logger) *Recorder {
	return &Recorder{store: st, log: log, prices: clonePrices(prices)}
}

func (r *Recorder) SetPrices(prices []Price) {
	r.mu.Lock()
	r.prices = clonePrices(prices)
	r.mu.Unlock()
}

// Record implements gateway.Recorder.
func (r *Recorder) Record(rec gateway.Record) {
	row := store.Request{
		At: rec.At, Key: rec.Key, Account: rec.Account, Model: rec.Model, Slug: rec.Slug,
		API: rec.API, Stream: rec.Stream, Mode: string(rec.Mode), Status: "ok", Finish: string(rec.Finish),
		Duration: rec.Duration, QuotaGroup: rec.QuotaGroup,
	}
	if rec.TimeToFirstOutput > 0 {
		ttft := rec.TimeToFirstOutput
		row.TTFT = &ttft
	}
	if rec.Err != nil {
		row.Status, row.Error = errorStatus(rec.Err.Kind), truncate(rec.Err.Message, 500)
	}
	if u := rec.Usage; u != nil {
		row.Input, row.Cached, row.Output, row.Reasoning = u.PromptTokens, u.CachedTokens, u.CompletionTokens, u.ReasoningTokens
		r.mu.Lock()
		price, ok := priceOf(r.prices, rec.Model)
		r.mu.Unlock()
		if ok {
			cost := price.cost(u.PromptTokens, u.CachedTokens, u.CompletionTokens)
			row.CostUSD = &cost
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := r.store.AddRequest(ctx, row); err != nil {
		r.log.Error("request record lost", "key", rec.Key, "err", err)
	}
}

var statusNames = map[conv.ErrorKind]string{
	conv.ErrInvalidRequest: "invalid_request",
	conv.ErrUnauthorized:   "unauthorized",
	conv.ErrForbidden:      "forbidden",
	conv.ErrUnknownModel:   "unknown_model",
	conv.ErrQuota:          "quota",
	conv.ErrKeyLimit:       "key_limit",
	conv.ErrOverloaded:     "overloaded",
	conv.ErrUnavailable:    "unavailable",
	conv.ErrUpstream:       "upstream_error",
}

func errorStatus(kind conv.ErrorKind) string {
	if name, ok := statusNames[kind]; ok {
		return name
	}
	return "error"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
