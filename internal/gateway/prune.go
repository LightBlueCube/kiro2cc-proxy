package gateway

import (
	"time"

	"agyrelay/internal/agy"
)

const (
	// conversationPruneEvery is how often agy's files of sessions nobody
	// can continue are deleted.
	conversationPruneEvery = 10 * time.Minute
	// conversationGrace keeps a session's files this long past its
	// snapshot's expiry and its last change: a request that just found the
	// snapshot, or a session agy is still creating, keeps them.
	conversationGrace = time.Hour
)

// pruneConversations deletes agy's files of every session that is neither
// open nor continuable from a snapshot: quota probes, sessions whose
// snapshot expired or was dropped. reopen replays a conversation whose
// files are gone.
func (g *Gateway) pruneConversations() {
	keep, err := g.snaps.live(conversationGrace)
	if err != nil {
		g.log.Warn("expired snapshots not removed from disk", "err", err)
	}
	g.mu.Lock()
	for ref := range g.sessions {
		keep[ref] = true
	}
	g.mu.Unlock()

	for _, account := range g.pool.Accounts() {
		stored, err := agy.Conversations(account.GeminiDir())
		if err != nil {
			g.log.Warn("listing stored sessions failed", "account", account.Name(), "err", err)
			continue
		}
		removed := 0
		for id, changed := range stored {
			if keep[sessionRef(account.Name(), id)] || time.Since(changed) < conversationGrace {
				continue
			}
			if err := agy.DeleteConversation(account.GeminiDir(), id); err != nil {
				g.log.Warn("deleting a stored session failed", "account", account.Name(), "session", id, "err", err)
				continue
			}
			removed++
		}
		if removed > 0 {
			g.log.Info("stored sessions deleted", "account", account.Name(), "count", removed, "kept", len(stored)-removed)
		}
	}
}
