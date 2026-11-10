package admin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"agyrelay/internal/logbuf"
)

const (
	defaultLogPage = 500
	maxLogPage     = 5000
	logHeartbeat   = 15 * time.Second
)

var levelNames = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

func logFilter(r *http.Request) (logbuf.Filter, bool) {
	q := r.URL.Query()
	f := logbuf.Filter{MinLevel: slog.LevelDebug, Component: q.Get("component"), Account: q.Get("account"), Text: q.Get("q")}
	if raw := q.Get("level"); raw != "" {
		level, ok := levelNames[raw]
		if !ok {
			return f, false
		}
		f.MinLevel = level
	}
	return f, true
}

// listLogs returns buffered entries after ?after (a sequence number).
func (h *Handler) listLogs(w http.ResponseWriter, r *http.Request) {
	f, ok := logFilter(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "level must be debug, info, warn, or error")
		return
	}
	q := r.URL.Query()
	after, limit := int64(0), defaultLogPage
	if raw := q.Get("after"); raw != "" {
		var err error
		if after, err = strconv.ParseInt(raw, 10, 64); err != nil {
			writeError(w, http.StatusBadRequest, "after must be a sequence number")
			return
		}
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLogPage {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 5000")
			return
		}
		limit = n
	}
	entries := h.logs.Since(after, f, limit)
	if entries == nil {
		entries = []logbuf.Entry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// streamLogs follows new matching entries as server-sent events.
func (h *Handler) streamLogs(w http.ResponseWriter, r *http.Request) {
	f, ok := logFilter(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "level must be debug, info, warn, or error")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	entries, cancel := h.logs.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	ticker := time.NewTicker(logHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
		case e := <-entries:
			if !f.Match(e) {
				continue
			}
			raw, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		flusher.Flush()
	}
}
