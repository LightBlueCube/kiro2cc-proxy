package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// snapshot records exactly what an ACP session holds: the first Count body
// messages of a client transcript whose prefix hash is Hash. Only the latest
// snapshot of a session is kept; older prefixes are never resumed, because
// the session has moved past them.
type snapshot struct {
	User       string    `json:"user"`
	Account    string    `json:"account"`
	SessionID  string    `json:"sessionId"`
	ToolsKey   string    `json:"toolsKey"`
	SystemHash string    `json:"systemHash"`
	Count      int       `json:"count"`
	Hash       string    `json:"hash"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// snapshotStore indexes snapshots by (user, prefix hash) and persists them
// so sessions survive a restart.
type snapshotStore struct {
	path string
	ttl  time.Duration

	saveMu    sync.Mutex
	mu        sync.Mutex
	bySession map[string]snapshot // account/sessionId → snapshot
	byPrefix  map[string]string   // user/hash → account/sessionId
}

func sessionRef(account, sessionID string) string { return account + "/" + sessionID }

func prefixRef(user, hash string) string { return user + "/" + hash }

func openSnapshotStore(path string, ttl time.Duration) (*snapshotStore, error) {
	s := &snapshotStore{path: path, ttl: ttl, bySession: map[string]snapshot{}, byPrefix: map[string]string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read snapshots: %w", err)
	}
	var list []snapshot
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse snapshots %s: %w", path, err)
	}
	now := time.Now()
	for _, snap := range list {
		if now.Sub(snap.UpdatedAt) > ttl {
			continue
		}
		s.putLocked(snap)
	}
	return s, nil
}

// lookup finds the user's longest snapshot that is a strict prefix of t's
// body (at least one message must follow it).
func (s *snapshotStore) lookup(user string, t *transcript) (snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n := len(t.body) - 1; n >= 1; n-- {
		ref, ok := s.byPrefix[prefixRef(user, t.hashes[n])]
		if !ok {
			continue
		}
		snap := s.bySession[ref]
		if snap.Count == n && time.Since(snap.UpdatedAt) <= s.ttl {
			return snap, true
		}
	}
	return snapshot{}, false
}

func (s *snapshotStore) put(snap snapshot) error {
	snap.UpdatedAt = time.Now()
	s.mu.Lock()
	s.putLocked(snap)
	s.mu.Unlock()
	return s.save()
}

func (s *snapshotStore) putLocked(snap snapshot) {
	ref := sessionRef(snap.Account, snap.SessionID)
	if old, ok := s.bySession[ref]; ok {
		delete(s.byPrefix, prefixRef(old.User, old.Hash))
	}
	s.bySession[ref] = snap
	s.byPrefix[prefixRef(snap.User, snap.Hash)] = ref
}

// drop forgets a session: its native state no longer matches any client
// transcript we know.
func (s *snapshotStore) drop(account, sessionID string) error {
	ref := sessionRef(account, sessionID)
	s.mu.Lock()
	old, ok := s.bySession[ref]
	if ok {
		delete(s.bySession, ref)
		delete(s.byPrefix, prefixRef(old.User, old.Hash))
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return s.save()
}

// save writes all snapshots atomically. Saves are serialized and each one
// reads the current state, so the last writer persists the newest state.
func (s *snapshotStore) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	list := make([]snapshot, 0, len(s.bySession))
	for _, snap := range s.bySession {
		list = append(list, snap)
	}
	s.mu.Unlock()
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".snapshots.*.tmp")
	if err != nil {
		return fmt.Errorf("save snapshots: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("save snapshots: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("save snapshots: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("save snapshots: %w", err)
	}
	return nil
}
