package gateway

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// snapshot records a position of an ACP session: it held the first Count
// body messages of a client transcript whose prefix hash is Hash. Steps is
// the session's native step count there when a finished turn left it there;
// the session can be rewound to such a position. 0 means unknown.
//
// A session keeps its latest position and the rewindable ones before it:
//
//	[n=2 steps=2] [n=4 steps=6] [n=5 latest, parked on a tool call]
type snapshot struct {
	User       string    `json:"user"`
	Account    string    `json:"account"`
	SessionID  string    `json:"sessionId"`
	ToolsKey   string    `json:"toolsKey"`
	SystemHash string    `json:"systemHash"`
	Count      int       `json:"count"`
	Hash       string    `json:"hash"`
	Steps      int       `json:"steps,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// snapshotStore indexes positions by (user, prefix hash) and persists them
// so sessions survive a restart. A session's age is that of its latest
// position.
type snapshotStore struct {
	path   string
	ttlNow atomic.Int64 // time.Duration

	saveMu    sync.Mutex
	mu        sync.Mutex
	bySession map[string][]snapshot // account/sessionId → positions by ascending Count
	byPrefix  map[string]string     // user/hash → account/sessionId
}

func sessionRef(account, sessionID string) string { return account + "/" + sessionID }

func prefixRef(user, hash string) string { return user + "/" + hash }

func openSnapshotStore(path string, ttl time.Duration) (*snapshotStore, error) {
	s := &snapshotStore{path: path, bySession: map[string][]snapshot{}, byPrefix: map[string]string{}}
	s.setTTL(ttl)
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
	for _, snap := range list {
		ref := sessionRef(snap.Account, snap.SessionID)
		s.bySession[ref] = append(s.bySession[ref], snap)
	}
	for ref, positions := range s.bySession {
		slices.SortFunc(positions, func(a, b snapshot) int { return cmp.Compare(a.Count, b.Count) })
		if s.expired(positions) {
			delete(s.bySession, ref)
			continue
		}
		for _, snap := range positions {
			s.byPrefix[prefixRef(snap.User, snap.Hash)] = ref
		}
	}
	return s, nil
}

func (s *snapshotStore) ttl() time.Duration { return time.Duration(s.ttlNow.Load()) }

func (s *snapshotStore) setTTL(ttl time.Duration) { s.ttlNow.Store(int64(ttl)) }

func (s *snapshotStore) expired(positions []snapshot) bool {
	return time.Since(positions[len(positions)-1].UpdatedAt) > s.ttl()
}

// lookup finds the user's longest position that is a strict prefix of t's
// body (at least one message must follow it).
func (s *snapshotStore) lookup(user string, t *transcript) (snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n := len(t.body) - 1; n >= 1; n-- {
		ref, ok := s.byPrefix[prefixRef(user, t.hashes[n])]
		if !ok {
			continue
		}
		if snap, ok := s.positionLocked(ref, n, t.hashes[n]); ok {
			return snap, true
		}
	}
	return snapshot{}, false
}

// position returns a session's position regardless of which user made it.
func (s *snapshotStore) position(account, sessionID string, count int, hash string) (snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.positionLocked(sessionRef(account, sessionID), count, hash)
}

func (s *snapshotStore) positionLocked(ref string, count int, hash string) (snapshot, bool) {
	positions := s.bySession[ref]
	if len(positions) == 0 || s.expired(positions) {
		return snapshot{}, false
	}
	for _, snap := range positions {
		if snap.Count == count && snap.Hash == hash {
			return snap, true
		}
	}
	return snapshot{}, false
}

// put makes snap the session's latest position. Earlier positions stay
// only if they can be rewound to.
func (s *snapshotStore) put(snap snapshot) error {
	snap.UpdatedAt = time.Now()
	ref := sessionRef(snap.Account, snap.SessionID)
	s.mu.Lock()
	s.keepLocked(ref, func(old snapshot) bool { return old.Count < snap.Count && old.Steps > 0 })
	s.bySession[ref] = append(s.bySession[ref], snap)
	s.byPrefix[prefixRef(snap.User, snap.Hash)] = ref
	s.mu.Unlock()
	return s.save()
}

// rewound makes the position with count messages the session's latest: the
// session was rewound to it.
func (s *snapshotStore) rewound(account, sessionID string, count int) error {
	s.mu.Lock()
	s.keepLocked(sessionRef(account, sessionID), func(old snapshot) bool { return old.Count <= count })
	s.mu.Unlock()
	return s.save()
}

// drop forgets a session: its native state no longer matches any client
// transcript we know.
func (s *snapshotStore) drop(account, sessionID string) error {
	ref := sessionRef(account, sessionID)
	s.mu.Lock()
	_, ok := s.bySession[ref]
	s.keepLocked(ref, func(snapshot) bool { return false })
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return s.save()
}

// keepLocked removes the session's positions that keep rejects.
func (s *snapshotStore) keepLocked(ref string, keep func(snapshot) bool) {
	var kept []snapshot
	for _, old := range s.bySession[ref] {
		if keep(old) {
			kept = append(kept, old)
			continue
		}
		key := prefixRef(old.User, old.Hash)
		if s.byPrefix[key] == ref {
			delete(s.byPrefix, key)
		}
	}
	if len(kept) == 0 {
		delete(s.bySession, ref)
		return
	}
	s.bySession[ref] = kept
}

// save writes all snapshots atomically. Saves are serialized and each one
// reads the current state, so the last writer persists the newest state.
func (s *snapshotStore) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	list := make([]snapshot, 0, len(s.bySession))
	for _, positions := range s.bySession {
		list = append(list, positions...)
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
