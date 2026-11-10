package admin

import (
	"net/http"
	"strconv"
	"time"

	"agyrelay/internal/store"
)

const (
	defaultRequestPage = 100
	maxRequestPage     = 500
)

type requestView struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	Key        string    `json:"key"`
	Account    string    `json:"account"`
	Model      string    `json:"model"`
	Slug       string    `json:"slug"`
	API        string    `json:"api"`
	Stream     bool      `json:"stream"`
	Mode       string    `json:"mode"`
	Status     string    `json:"status"`
	Error      string    `json:"error"`
	Finish     string    `json:"finish"`
	DurationMS int64     `json:"duration_ms"`
	TTFTMS     *int64    `json:"ttft_ms"`
	Input      int64     `json:"input"`
	Cached     int64     `json:"cached"`
	Output     int64     `json:"output"`
	Reasoning  int64     `json:"reasoning"`
	CostUSD    *float64  `json:"cost_usd"`
	QuotaGroup string    `json:"quota_group"`
	QuotaUsed  *float64  `json:"quota_used"`
}

// queryFilter reads key, model, account, and since/until (Unix ms).
func queryFilter(r *http.Request) (store.RequestFilter, bool) {
	q := r.URL.Query()
	f := store.RequestFilter{Key: q.Get("key"), Model: q.Get("model"), Account: q.Get("account")}
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		raw := q.Get(name)
		if raw == "" {
			continue
		}
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return f, false
		}
		*dst = time.UnixMilli(ms)
	}
	return f, true
}

func (h *Handler) listRequests(w http.ResponseWriter, r *http.Request) {
	f, ok := queryFilter(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "since and until are Unix milliseconds")
		return
	}
	q := r.URL.Query()
	before, limit := int64(0), defaultRequestPage
	if raw := q.Get("before"); raw != "" {
		var err error
		if before, err = strconv.ParseInt(raw, 10, 64); err != nil {
			writeError(w, http.StatusBadRequest, "before must be a request id")
			return
		}
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRequestPage {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	rows, err := h.store.Requests(r.Context(), f, before, limit)
	if err != nil {
		h.internalError(w, "list requests", err)
		return
	}
	out := []requestView{}
	for _, row := range rows {
		v := requestView{
			ID: row.ID, At: row.At, Key: row.Key, Account: row.Account, Model: row.Model, Slug: row.Slug, API: row.API,
			Stream: row.Stream, Mode: row.Mode, Status: row.Status, Error: row.Error, Finish: row.Finish,
			DurationMS: row.Duration.Milliseconds(), Input: row.Input, Cached: row.Cached, Output: row.Output,
			Reasoning: row.Reasoning, CostUSD: row.CostUSD, QuotaGroup: row.QuotaGroup, QuotaUsed: row.QuotaUsed,
		}
		if row.TTFT != nil {
			ms := row.TTFT.Milliseconds()
			v.TTFTMS = &ms
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// summarize serves totals grouped by ?by=key|model|account|day|day_key;
// days use the browser's zone, ?tz = minutes east of UTC.
func (h *Handler) summarize(w http.ResponseWriter, r *http.Request) {
	f, ok := queryFilter(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "since and until are Unix milliseconds")
		return
	}
	q := r.URL.Query()
	tz := 0
	if raw := q.Get("tz"); raw != "" {
		var err error
		if tz, err = strconv.Atoi(raw); err != nil || tz < -14*60 || tz > 14*60 {
			writeError(w, http.StatusBadRequest, "tz is minutes east of UTC")
			return
		}
	}
	by := store.SummaryBy(q.Get("by"))
	switch by {
	case store.ByKey, store.ByModel, store.ByAccount, store.ByDay, store.ByDayKey:
	default:
		writeError(w, http.StatusBadRequest, "by must be key, model, account, day, or day_key")
		return
	}
	sums, err := h.store.Summarize(r.Context(), f, by, time.Duration(tz)*time.Minute)
	if err != nil {
		h.internalError(w, "summarize requests", err)
		return
	}
	if sums == nil {
		sums = []store.Summary{}
	}
	writeJSON(w, http.StatusOK, sums)
}

// filterValues lists the keys, models, and accounts seen since ?since.
func (h *Handler) filterValues(w http.ResponseWriter, r *http.Request) {
	f, ok := queryFilter(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "since is Unix milliseconds")
		return
	}
	out := map[string][]string{}
	for _, by := range []store.SummaryBy{store.ByKey, store.ByModel, store.ByAccount} {
		values, err := h.store.Distinct(r.Context(), by, f.Since)
		if err != nil {
			h.internalError(w, "list filter values", err)
			return
		}
		if values == nil {
			values = []string{}
		}
		out[string(by)] = values
	}
	writeJSON(w, http.StatusOK, out)
}
