package agy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// What agy 1.5.3 keeps per session below <gemini-dir>, the session id
// being the cascade id (seen on a live account; every conversations entry
// had a brain entry):
//
//	antigravity-cli/conversations/<id>.db (and -wal, -shm, -journal)
//	antigravity-cli/brain/<id>/  transcripts, uploaded images, scratch files
//	antigravity-cli/annotations/<id>.pbtxt
//
// conversation_summaries.db also holds a row per session; it stays, since
// running processes keep that database open.
const (
	conversationsDir = "antigravity-cli/conversations"
	brainDir         = "antigravity-cli/brain"
	annotationsDir   = "antigravity-cli/annotations"
	logDir           = "antigravity-cli/log"
	crashDir         = "antigravity-cli/crashes"
)

// logName is a process log, acp-<kind>-<YYYYMMDD_HHMMSS>-<pid>.log.
var logName = regexp.MustCompile(`^acp-[a-z]+-\d{8}_\d{6}-(\d+)\.log$`)

// Conversations lists the sessions stored below geminiDir with the last
// change of each.
func Conversations(geminiDir string) (map[string]time.Time, error) {
	entries, err := os.ReadDir(filepath.Join(geminiDir, conversationsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	stored := map[string]time.Time{}
	for _, entry := range entries {
		id, _, isDB := strings.Cut(entry.Name(), ".db")
		if !isDB {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, err
		}
		if info.ModTime().After(stored[id]) {
			stored[id] = info.ModTime()
		}
	}
	return stored, nil
}

// DeleteConversation removes a session's files. No process may hold the
// session open.
func DeleteConversation(geminiDir, sessionID string) error {
	db := filepath.Join(geminiDir, conversationsDir, sessionID+".db")
	paths := []string{db, db + "-wal", db + "-shm", db + "-journal",
		filepath.Join(geminiDir, annotationsDir, sessionID+".pbtxt")}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.RemoveAll(filepath.Join(geminiDir, brainDir, sessionID))
}

// PruneLogs deletes process logs and crash reports last written before
// cutoff, except the logs of processes still running, and returns how many
// went.
func PruneLogs(geminiDir string, cutoff time.Time) (int, error) {
	removed := 0
	for _, dir := range []string{logDir, crashDir} {
		entries, err := os.ReadDir(filepath.Join(geminiDir, dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return removed, err
			}
			if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) || ofRunningProcess(entry.Name()) {
				continue
			}
			if err := os.Remove(filepath.Join(geminiDir, dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// ofRunningProcess reports whether name is the log of a live process: the
// relay reads the logs of running agy processes to find their language
// servers and workers.
func ofRunningProcess(name string) bool {
	m := logName.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
