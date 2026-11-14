package agy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestConversationFiles(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-time.Minute)
	conv := filepath.Join(dir, conversationsDir)
	touch(t, filepath.Join(conv, "a.db"), old)
	touch(t, filepath.Join(conv, "a.db-wal"), recent)
	touch(t, filepath.Join(conv, "b.db"), old)
	touch(t, filepath.Join(conv, "notes.txt"), old)
	touch(t, filepath.Join(dir, brainDir, "a", ".system_generated", "logs", "transcript.jsonl"), old)
	touch(t, filepath.Join(dir, annotationsDir, "a.pbtxt"), old)
	touch(t, filepath.Join(dir, brainDir, "b", "scratch", "x"), old)

	stored, err := Conversations(dir)
	if err != nil {
		t.Fatal(err)
	}
	// a's newest file is its WAL.
	if len(stored) != 2 || stored["a"].Sub(recent).Abs() > time.Second || stored["b"].Sub(old).Abs() > time.Second {
		t.Fatalf("stored = %v", stored)
	}

	if err := DeleteConversation(dir, "a"); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"a.db", "a.db-wal"} {
		if exists(filepath.Join(conv, gone)) {
			t.Errorf("%s left", gone)
		}
	}
	if exists(filepath.Join(dir, brainDir, "a")) || exists(filepath.Join(dir, annotationsDir, "a.pbtxt")) {
		t.Error("brain or annotations of a left")
	}
	if !exists(filepath.Join(conv, "b.db")) || !exists(filepath.Join(dir, brainDir, "b", "scratch", "x")) || !exists(filepath.Join(conv, "notes.txt")) {
		t.Error("b was touched")
	}
	if stored, _ := Conversations(t.TempDir()); stored != nil {
		t.Errorf("no conversations dir: %v", stored)
	}
}

func TestPruneLogs(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-10 * 24 * time.Hour)
	logs := filepath.Join(dir, logDir)
	running := fmt.Sprintf("acp-front-20260101_000000-%d.log", os.Getpid())
	touch(t, filepath.Join(logs, running), old)
	touch(t, filepath.Join(logs, "acp-worker-20260101_000000-999999999.log"), old)
	touch(t, filepath.Join(logs, "acp-worker-20260101_000001-999999998.log"), time.Now())
	touch(t, filepath.Join(dir, crashDir, "crash_1_x.log"), old)

	removed, err := PruneLogs(dir, time.Now().Add(-7*24*time.Hour))
	if err != nil || removed != 2 {
		t.Fatalf("removed %d, %v", removed, err)
	}
	if !exists(filepath.Join(logs, running)) || !exists(filepath.Join(logs, "acp-worker-20260101_000001-999999998.log")) {
		t.Error("a running process's log or a recent log went")
	}
	if exists(filepath.Join(logs, "acp-worker-20260101_000000-999999999.log")) || exists(filepath.Join(dir, crashDir, "crash_1_x.log")) {
		t.Error("old logs stayed")
	}
}

func TestLiveWorkersUnassignedSessions(t *testing.T) {
	pid := os.Getpid()
	_, group, err := readResources(pid)
	if err != nil {
		t.Fatal(err)
	}
	front := fmt.Sprintf("I1114 10:04:17.812704 11 front.go:1064] front: started worker 1 (pid %d)\n", pid)
	workers := liveWorkers(front, group)
	if len(workers) != 1 || workers[0].Sessions == nil || len(workers[0].Sessions) != 0 {
		t.Fatalf("workers = %#v, want non-nil empty Sessions slice", workers)
	}
}
