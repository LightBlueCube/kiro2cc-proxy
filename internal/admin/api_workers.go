package admin

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"agyrelay/internal/agy"
)

type workersView struct {
	Accounts []accountWorkersView `json:"accounts"`
	// Now is the server's time, so ages do not depend on the browser clock.
	Now time.Time `json:"now"`
}

type accountWorkersView struct {
	Name     string        `json:"name"`
	Paused   bool          `json:"paused"`
	Disabled string        `json:"disabled"`
	Blocked  string        `json:"blocked"`
	Verify   bool          `json:"verify"`
	Turns    int           `json:"turns"`
	Procs    []procView    `json:"procs"`
	Sessions []sessionView `json:"sessions"`
	Egress   *egressView   `json:"egress"`
}

type procView struct {
	PID        int          `json:"pid"`
	Agent      string       `json:"agent"`
	Started    time.Time    `json:"started"`
	RSSBytes   int64        `json:"rss_bytes"`
	CPUSeconds float64      `json:"cpu_seconds"`
	Workers    []workerView `json:"workers"`
	Error      string       `json:"error"`
}

type workerView struct {
	Number     int      `json:"number"`
	PID        int      `json:"pid"`
	RSSBytes   int64    `json:"rss_bytes"`
	CPUSeconds float64  `json:"cpu_seconds"`
	Sessions   []string `json:"sessions"`
}

type sessionView struct {
	ID string `json:"id"`
	// Key is the key's name, "" once the key is deleted.
	Key      string    `json:"key"`
	KeyHash  string    `json:"key_hash"`
	PID      int       `json:"pid"`
	Worker   int       `json:"worker"`
	LastUsed time.Time `json:"last_used"`
	Turn     *turnView `json:"turn"`
}

type turnView struct {
	State       string     `json:"state"`
	Model       string     `json:"model"`
	Started     *time.Time `json:"started"`
	ParkedAt    *time.Time `json:"parked_at"`
	ParkTimeout *time.Time `json:"park_timeout"`
}

type egressView struct {
	Route       string         `json:"route"`
	Open        int            `json:"open"`
	Connections int64          `json:"connections"`
	BytesOut    int64          `json:"bytes_out"`
	BytesIn     int64          `json:"bytes_in"`
	Refused     int64          `json:"refused"`
	LastError   string         `json:"last_error"`
	LastErrorAt *time.Time     `json:"last_error_at"`
	Hosts       []hostStatView `json:"hosts"`
}

type hostStatView struct {
	Host        string    `json:"host"`
	Connections int64     `json:"connections"`
	LastAt      time.Time `json:"last_at"`
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (h *Handler) listWorkers(w http.ResponseWriter, r *http.Request) {
	keys, err := h.control.Keys(r.Context())
	if err != nil {
		h.internalError(w, "list keys", err)
		return
	}
	keyNames := map[string]string{}
	for _, k := range keys {
		keyNames[k.Hash] = k.Name
	}

	sessionsByAccount := map[string][]sessionView{}
	for _, s := range h.control.Sessions() {
		v := sessionView{ID: s.ID, Key: keyNames[s.KeyHash], KeyHash: s.KeyHash, PID: s.PID, LastUsed: s.LastUsed}
		if t := s.Turn; t != nil {
			v.Turn = &turnView{State: string(t.State), Model: t.Model, Started: optionalTime(t.Started),
				ParkedAt: optionalTime(t.ParkedAt), ParkTimeout: optionalTime(t.ParkTimeout)}
		}
		sessionsByAccount[s.Account] = append(sessionsByAccount[s.Account], v)
	}

	out := workersView{Accounts: []accountWorkersView{}, Now: time.Now()}
	for _, account := range h.pool.Accounts() {
		st := account.Status()
		v := accountWorkersView{Name: st.Name, Paused: st.Paused, Disabled: st.Disabled, Blocked: st.Blocked, Verify: st.Verify, Turns: st.Turns,
			Procs: []procView{}, Sessions: sessionsByAccount[st.Name]}
		workerOf := map[string]int{}
		for _, proc := range account.LiveProcs() {
			info, err := proc.Inspect()
			pv := procView{PID: info.PID, Agent: info.Agent, Started: info.Started,
				RSSBytes: info.Resources.RSSBytes, CPUSeconds: info.Resources.CPUSeconds, Workers: []workerView{}}
			if err != nil {
				pv.Error = err.Error()
			}
			for _, wk := range info.Workers {
				sessions := wk.Sessions
				if sessions == nil {
					sessions = []string{}
				}
				pv.Workers = append(pv.Workers, workerView{Number: wk.Number, PID: wk.PID, RSSBytes: wk.Resources.RSSBytes,
					CPUSeconds: wk.Resources.CPUSeconds, Sessions: sessions})
				for _, id := range sessions {
					workerOf[id] = wk.Number
				}
			}
			v.Procs = append(v.Procs, pv)
		}
		if v.Sessions == nil {
			v.Sessions = []sessionView{}
		}
		for i := range v.Sessions {
			v.Sessions[i].Worker = workerOf[v.Sessions[i].ID]
		}
		if traffic, ok := h.control.Egress(st.Name); ok {
			ev := &egressView{Route: traffic.Route, Open: traffic.Open, Connections: traffic.Connections, BytesOut: traffic.BytesOut,
				BytesIn: traffic.BytesIn, Refused: traffic.Refused, LastError: traffic.LastError, LastErrorAt: optionalTime(traffic.LastErrorAt),
				Hosts: []hostStatView{}}
			for _, host := range traffic.Hosts {
				ev.Hosts = append(ev.Hosts, hostStatView{Host: host.Host, Connections: host.Connections, LastAt: host.LastAt})
			}
			v.Egress = ev
		}
		out.Accounts = append(out.Accounts, v)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].Name < out.Accounts[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) cancelTurn(w http.ResponseWriter, r *http.Request) {
	account, id := r.PathValue("account"), r.PathValue("id")
	if err := h.control.CancelTurn(account, id); err != nil {
		h.fail(w, "cancel turn", err)
		return
	}
	h.audit(r, "turn_cancelled", account+"/"+id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) closeSession(w http.ResponseWriter, r *http.Request) {
	account, id := r.PathValue("account"), r.PathValue("id")
	if err := h.control.CloseSession(r.Context(), account, id); err != nil {
		h.fail(w, "close session", err)
		return
	}
	h.audit(r, "session_closed", account+"/"+id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

type agyLogFileView struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	PID      int       `json:"pid"`
	Running  bool      `json:"running"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func (h *Handler) listAgyLogs(w http.ResponseWriter, r *http.Request) {
	files, err := h.control.AgyLogFiles(r.PathValue("account"))
	if err != nil {
		h.fail(w, "list agy logs", err)
		return
	}
	out := []agyLogFileView{}
	for _, f := range files {
		out = append(out, agyLogFileView{Name: f.Name, Kind: f.Kind, PID: f.PID, Running: f.Running, Size: f.Size, Modified: f.Modified})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

const (
	defaultAgyLogEntries = 500
	maxAgyLogEntries     = 5000
)

type agyLogEntryView struct {
	File  string `json:"file"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

// agyLogEntries answers ?file=&level=&q=&limit=: level is the lowest
// glog severity (I, W, E, F), empty for every line.
func (h *Handler) agyLogEntries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q := agy.LogQuery{File: query.Get("file"), MinLevel: query.Get("level"), Contains: query.Get("q"), Limit: defaultAgyLogEntries}
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxAgyLogEntries {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1 to %d", maxAgyLogEntries))
			return
		}
		q.Limit = n
	}
	result, err := h.control.AgyLogs(r.PathValue("account"), q)
	if err != nil {
		h.fail(w, "read agy logs", err)
		return
	}
	entries := make([]agyLogEntryView, len(result.Entries))
	for i, e := range result.Entries {
		entries[i] = agyLogEntryView{File: e.File, Level: e.Level, Text: e.Text}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "matched": result.Matched, "partial": result.Partial})
}

func (h *Handler) restartAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("account")
	if err := h.control.RestartAccount(name); err != nil {
		h.fail(w, "restart account", err)
		return
	}
	h.audit(r, "account_restarted", name)
	writeJSON(w, http.StatusOK, map[string]any{})
}
