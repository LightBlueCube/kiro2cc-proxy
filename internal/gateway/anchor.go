package gateway

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"agyrelay/internal/conv"
)

// An anchor names the native position an assistant reply left its session
// at, so a client that returns it lets the planner find the session without
// matching its own key-scoped snapshots. It is plain data, not a secret:
// whoever holds an anchor may continue that session.
//
//	agr1.<base64url(JSON)>   e.g. {"a":"main","s":"<sessionId>","n":4,"h":"<prefix hash>"}
const anchorPrefix = "agr1."

type anchor struct {
	Account   string `json:"a"`
	SessionID string `json:"s"`
	// Count and Hash are the transcript prefix the session held after the
	// reply, as in snapshot.
	Count int    `json:"n"`
	Hash  string `json:"h"`
}

func (a anchor) encode() string {
	raw, _ := json.Marshal(a)
	return anchorPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// parseAnchor accepts only well-formed values of this format; anything else
// (another provider's reasoning data, a damaged copy) is not an anchor.
func parseAnchor(value string) (anchor, bool) {
	payload, ok := strings.CutPrefix(strings.TrimSpace(value), anchorPrefix)
	if !ok {
		return anchor{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return anchor{}, false
	}
	var a anchor
	if json.Unmarshal(raw, &a) != nil || a.Account == "" || a.SessionID == "" || a.Count < 1 || a.Hash == "" {
		return anchor{}, false
	}
	return a, true
}

// anchoredSnapshot finds the session named by the request's furthest
// usable anchor: the request must still contain the anchored prefix, a
// message must follow it, and the session must still hold that position
// or an earlier rewindable prefix of it (e.g. the settled input before an
// unrewindable tool-call reply whose turn already moved past it).
// Where in the transcript the client put the anchor does not matter.
func (g *Gateway) anchoredSnapshot(t *transcript) (snapshot, bool) {
	best := snapshot{}
	found := false
	for _, m := range t.body {
		for _, value := range m.Anchors {
			a, ok := parseAnchor(value)
			if !ok || a.Count >= len(t.body) || t.hashes[a.Count] != a.Hash || (found && a.Count <= best.Count) {
				continue
			}
			snap, ok := g.snaps.prefixInSession(a.Account, a.SessionID, t, a.Count)
			if !ok || (found && snap.Count <= best.Count) {
				continue
			}
			best, found = snap, true
		}
	}
	return best, found
}

// inspectAnchor checks whether the request returned a valid agr1. anchor
// covering its latest assistant turn.
func inspectAnchor(t *transcript) AnchorStatus {
	lastAsst := -1
	anyAnchor := false
	for i, m := range t.body {
		if m.Role == conv.RoleAssistant {
			lastAsst = i
		}
		if len(m.Anchors) > 0 {
			anyAnchor = true
		}
	}
	if lastAsst < 0 {
		return AnchorNone
	}
	bestCount := 0
	for _, m := range t.body {
		for _, value := range m.Anchors {
			a, ok := parseAnchor(value)
			if !ok || a.Count > len(t.body) || t.hashes[a.Count] != a.Hash {
				continue
			}
			if a.Count > bestCount {
				bestCount = a.Count
			}
		}
	}
	if bestCount >= lastAsst+1 {
		return AnchorOK
	}
	if bestCount > 0 {
		return AnchorStale
	}
	if anyAnchor {
		return AnchorInvalid
	}
	return AnchorMissing
}
