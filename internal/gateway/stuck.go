package gateway

import "agyrelay/internal/pool"

// StopStuckSession ends the running turn of a session that keeps failing
// generation calls (pool.CallStuck): agy would retry without end. The
// turn's request fails as its last call failed, last.
func (g *Gateway) StopStuckSession(account *pool.Account, sessionID string, last pool.ModelFailure) {
	g.mu.Lock()
	sess := g.sessions[sessionRef(account.ID(), sessionID)]
	if sess == nil || sess.turn == nil || sess.turn.turn == nil {
		g.mu.Unlock()
		g.log.Warn("stuck session has no running turn", "account", account.Name(), "session", sessionID)
		return
	}
	sess.turn.stuck = &last
	turn := sess.turn.turn
	g.mu.Unlock()
	turn.Cancel()
}
