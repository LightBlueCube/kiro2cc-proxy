package admin

import (
	"cmp"
	"net/http"
	"time"

	"agyrelay/internal/store"
)

type flaggedPromptView struct {
	Fingerprint string    `json:"fingerprint"`
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	Until       time.Time `json:"until"`
	// Active is whether the prompt is being refused: pinned, or its flag
	// is still running.
	Active  bool   `json:"active"`
	Pinned  bool   `json:"pinned"`
	Hits    int    `json:"hits"`
	KeyHash string `json:"key_hash"`
	KeyName string `json:"key_name"`
	Account string `json:"account"`
	Model   string `json:"model"`
}

func (h *Handler) listFlaggedPrompts(w http.ResponseWriter, r *http.Request) {
	prompts, err := h.control.FlaggedPrompts(r.Context())
	if err != nil {
		h.internalError(w, "list flagged prompts", err)
		return
	}
	now := time.Now()
	out := struct {
		Prompts []flaggedPromptView `json:"prompts"`
	}{Prompts: []flaggedPromptView{}}
	for _, p := range prompts {
		out.Prompts = append(out.Prompts, buildFlaggedPromptView(p, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func buildFlaggedPromptView(p store.FlaggedPrompt, now time.Time) flaggedPromptView {
	return flaggedPromptView{
		Fingerprint: p.Fingerprint, FirstAt: p.FirstAt, LastAt: p.LastAt, Until: p.Until, Active: p.Pinned || now.Before(p.Until), Pinned: p.Pinned,
		Hits: p.Hits, KeyHash: p.KeyHash, KeyName: p.KeyName, Account: cmp.Or(p.AccountName, p.Account), Model: p.Model,
	}
}

type flaggedPromptDetailView struct {
	flaggedPromptView
	Session string            `json:"session,omitempty"`
	Text    string            `json:"text"`
	Call    *upstreamCallView `json:"call,omitempty"`
}

func (h *Handler) getFlaggedPrompt(w http.ResponseWriter, r *http.Request) {
	p, err := h.control.FlaggedPrompt(r.Context(), r.PathValue("fingerprint"))
	if err != nil {
		h.fail(w, "read flagged prompt", err)
		return
	}
	out := flaggedPromptDetailView{
		flaggedPromptView: buildFlaggedPromptView(p, time.Now()),
		Session:           p.Session,
		Text:              p.Text,
	}
	if len(p.Request) > 0 || len(p.Response) > 0 {
		call := buildUpstreamCallView(store.UpstreamCall{
			At: p.LastAt, Account: p.Account, AccountName: p.AccountName, Session: p.Session, Model: p.Model,
			Status: http.StatusTooManyRequests, Request: p.Request, Response: p.Response,
		})
		out.Call = &call
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) cancelFlaggedPrompt(w http.ResponseWriter, r *http.Request) {
	fingerprint := r.PathValue("fingerprint")
	if err := h.control.CancelFlaggedPrompt(r.Context(), fingerprint); err != nil {
		h.fail(w, "cancel flagged prompt", err)
		return
	}
	h.audit(r, "flagged_prompt_cancelled", fingerprint)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *Handler) pinFlaggedPrompt(pin bool) http.HandlerFunc {
	action := "flagged_prompt_unpinned"
	if pin {
		action = "flagged_prompt_pinned"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		fingerprint := r.PathValue("fingerprint")
		apply := h.control.UnpinFlaggedPrompt
		if pin {
			apply = h.control.PinFlaggedPrompt
		}
		if err := apply(r.Context(), fingerprint); err != nil {
			h.fail(w, "pin flagged prompt", err)
			return
		}
		h.audit(r, action, fingerprint)
		writeJSON(w, http.StatusOK, map[string]any{})
	}
}

func (h *Handler) deleteFlaggedPrompt(w http.ResponseWriter, r *http.Request) {
	fingerprint := r.PathValue("fingerprint")
	if err := h.control.DeleteFlaggedPrompt(r.Context(), fingerprint); err != nil {
		h.fail(w, "delete flagged prompt", err)
		return
	}
	h.audit(r, "flagged_prompt_deleted", fingerprint)
	writeJSON(w, http.StatusOK, map[string]any{})
}
