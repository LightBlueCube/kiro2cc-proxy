package agy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agyrelay/internal/agy/agytest"
)

const (
	envFrontIsolationStub    = "TEST_AGY_FRONT_STUB"
	envFrontHangOnWorkerExit = "TEST_AGY_FRONT_HANG"
	envWorkerSleepStub       = "TEST_AGY_WORKER_SLEEP"
)

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(agytest.EnvRun) == "1":
		agytest.Main()
		os.Exit(0)
	case os.Getenv(envWorkerSleepStub) == "1":
		select {}
	case os.Getenv(envFrontIsolationStub) == "1":
		runIsolatedFrontStub()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runIsolatedFrontStub mimics `agy acp --process-per-session` with two real
// child worker processes in the front's process group. Session s1 ignores
// session/cancel and fails its prompt once worker 1 exits (unless
// envFrontHangOnWorkerExit is set); session s2 answers prompts normally.
func runIsolatedFrontStub() {
	var geminiDir string
	for i, arg := range os.Args {
		if arg == "--gemini-dir" && i+1 < len(os.Args) {
			geminiDir = os.Args[i+1]
		}
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	spawnWorker := func() (*exec.Cmd, chan struct{}) {
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), envWorkerSleepStub+"=1")
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		return cmd, done
	}
	w1, w1Done := spawnWorker()
	w2, _ := spawnWorker()
	defer func() {
		_ = w1.Process.Kill()
		_ = w2.Process.Kill()
	}()

	logPath := filepath.Join(geminiDir, "antigravity-cli", "log", fmt.Sprintf("acp-front-20260101_000000-%d.log", os.Getpid()))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		os.Exit(2)
	}
	frontLog := fmt.Sprintf(
		"I0101 00:00:00.000000 1 front.go:1] front: started worker 1 (pid %d)\n"+
			"I0101 00:00:00.000001 1 front.go:1] front: started worker 2 (pid %d)\n"+
			"I0101 00:00:00.000002 1 front.go:1] front: session s1 served by worker 1\n"+
			"I0101 00:00:00.000003 1 front.go:1] front: session s2 served by worker 2\n",
		w1.Process.Pid, w2.Process.Pid,
	)
	if err := os.WriteFile(logPath, []byte(frontLog), 0o600); err != nil {
		os.Exit(2)
	}

	var outMu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		raw, _ := json.Marshal(v)
		outMu.Lock()
		defer outMu.Unlock()
		_, _ = out.Write(raw)
		_ = out.WriteByte('\n')
		_ = out.Flush()
	}

	hangOnExit := os.Getenv(envFrontHangOnWorkerExit) == "1"
	nextSession := 0
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Method == "" {
			continue
		}
		switch msg.Method {
		case "initialize":
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"protocolVersion":   1,
					"agentInfo":         map[string]string{"name": "stub-front", "version": "1.5.3"},
					"agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}},
				},
			})
		case "session/new":
			nextSession++
			id := fmt.Sprintf("s%d", nextSession)
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result":  map[string]any{"sessionId": id, "models": map[string]any{"availableModels": agytest.Models}},
			})
		case "session/set_config_option":
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{}})
		case "session/prompt":
			var p struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			id := msg.ID
			if p.SessionID == "s1" {
				go func() {
					if hangOnExit {
						return
					}
					<-w1Done
					send(map[string]any{
						"jsonrpc": "2.0",
						"id":      id,
						"error":   map[string]any{"code": -32603, "message": "worker 1 exited"},
					})
				}()
			} else {
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"stopReason": StopEndTurn},
				})
			}
		case "session/cancel":
			// Intentionally ignored so cancelGrace exercises forceStop.
		}
	}
}

type loggedRPC struct {
	Method string `json:"method"`
	Params struct {
		SessionID string `json:"sessionId"`
	} `json:"params"`
}

func readRPCLog(t *testing.T, path string) []loggedRPC {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []loggedRPC
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry loggedRPC
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

func startFakeProc(t *testing.T, extraEnv ...string) (*Proc, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rpc.jsonl")
	env := append([]string{agytest.EnvRun + "=1", "FAKE_AGY_LOG=" + logPath}, extraEnv...)
	proc, err := StartProc(context.Background(), ProcConfig{
		Bin:       exe,
		Home:      dir,
		GeminiDir: filepath.Join(dir, "gemini"),
		Workdir:   dir,
		Env:       env,
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		proc.Kill()
		<-proc.Done()
	})
	return proc, logPath
}

func TestCancelDuringSetConfigOptionSkipsPrompt(t *testing.T) {
	proc, logPath := startFakeProc(t, "FAKE_AGY_CONFIG_DELAY=150ms")
	sessID, _, err := proc.NewSession(context.Background(), SessionSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	turn := proc.RunTurn(TurnSpec{
		SessionID: sessID,
		Model:     "gemini-3.8-flash-low",
		Prompt:    []ContentBlock{TextBlock("hello")},
	})
	time.Sleep(25 * time.Millisecond)
	turn.Cancel()

	select {
	case <-turn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not settle after cancel during SetConfigOption")
	}
	if elapsed := time.Since(start); elapsed > 120*time.Millisecond {
		t.Fatalf("cancel waited %v for SetConfigOption to finish", elapsed)
	}
	if out := turn.Outcome(); out.Err != nil || out.StopReason != StopCancelled {
		t.Fatalf("outcome = %+v, want StopCancelled", out)
	}
	if !proc.Alive() {
		t.Fatal("proc was killed by early cancel")
	}

	// Wait past the fake's config delay to confirm no late prompt was written.
	time.Sleep(180 * time.Millisecond)
	for _, entry := range readRPCLog(t, logPath) {
		if entry.Method == "session/prompt" || entry.Method == "session/cancel" {
			t.Fatalf("unexpected RPC %q sent after early cancel", entry.Method)
		}
	}

	// Session stays healthy for a subsequent turn.
	next := proc.RunTurn(TurnSpec{
		SessionID: sessID,
		Model:     "gemini-3.8-flash-low",
		Prompt:    []ContentBlock{TextBlock("after cancel")},
	})
	select {
	case <-next.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("subsequent turn did not finish")
	}
	if out := next.Outcome(); out.Err != nil || out.StopReason != StopEndTurn {
		t.Fatalf("subsequent outcome = %+v, want StopEndTurn", out)
	}
}

func TestCancelRaceNeverPrecedesPrompt(t *testing.T) {
	proc, logPath := startFakeProc(t)
	sessID, _, err := proc.NewSession(context.Background(), SessionSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	for i := range 20 {
		turn := proc.RunTurn(TurnSpec{
			SessionID: sessID,
			Model:     "gemini-3.8-flash-low",
			Prompt:    []ContentBlock{TextBlock("@hang")},
		})
		if i%2 == 0 {
			time.Sleep(time.Duration(i%5) * time.Millisecond)
		}
		turn.Cancel()
		select {
		case <-turn.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: turn did not settle", i)
		}
		if out := turn.Outcome(); out.Err != nil || out.StopReason != StopCancelled {
			t.Fatalf("iteration %d: outcome = %+v, want StopCancelled", i, out)
		}
	}
	if !proc.Alive() {
		t.Fatal("proc died during rapid RunTurn/Cancel cycles")
	}

	promptsSeen := 0
	for _, entry := range readRPCLog(t, logPath) {
		if entry.Method == "session/prompt" {
			promptsSeen++
		}
		if entry.Method == "session/cancel" && promptsSeen == 0 {
			t.Fatal("session/cancel was written before any session/prompt")
		}
	}
}

func TestCancelledContextDoesNotSendRPC(t *testing.T) {
	proc, logPath := startFakeProc(t)
	sessID, _, err := proc.NewSession(context.Background(), SessionSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	before := len(readRPCLog(t, logPath))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := proc.NewSession(ctx, SessionSpec{Cwd: t.TempDir()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewSession(cancelled) err = %v, want context.Canceled", err)
	}
	if err := proc.ReopenSession(ctx, sessID, SessionSpec{Cwd: t.TempDir()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReopenSession(cancelled) err = %v, want context.Canceled", err)
	}
	if err := proc.SetConfigOption(ctx, sessID, "model", "gemini-3.8-flash-low"); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetConfigOption(cancelled) err = %v, want context.Canceled", err)
	}

	if after := readRPCLog(t, logPath); len(after) != before {
		t.Fatalf("cancelled RPCs were sent to agy: %+v", after[before:])
	}
}

func TestTurnNextStopsImmediatelyOnCancelledContext(t *testing.T) {
	turn := &Turn{
		events: []Event{{Kind: EventText, Text: "buffered"}},
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ev, ok := turn.Next(ctx); ok {
		t.Fatalf("Next(cancelled) returned event %+v, want false", ev)
	}
}

func TestForceStopKillsOnlyStuckWorkerWhenIsolated(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	proc, err := StartProc(context.Background(), ProcConfig{
		Bin:       exe,
		Home:      dir,
		GeminiDir: filepath.Join(dir, "gemini"),
		Workdir:   dir,
		Env:       []string{envFrontIsolationStub + "=1"},
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		proc.Kill()
		<-proc.Done()
	})

	s1, _, err := proc.NewSession(context.Background(), SessionSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	s2, _, err := proc.NewSession(context.Background(), SessionSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	w2PID, err := proc.workerPID(s2)
	if err != nil {
		t.Fatal(err)
	}

	turn1 := proc.runTurn(TurnSpec{SessionID: s1, Model: "gemini-3.8-flash-low", Prompt: []ContentBlock{TextBlock("hang")}}, 50*time.Millisecond, 500*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	turn1.Cancel()

	select {
	case <-turn1.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stuck turn did not settle after worker kill")
	}
	if out := turn1.Outcome(); out.Err != nil || out.StopReason != StopCancelled {
		t.Fatalf("turn1 outcome = %+v, want StopCancelled", out)
	}
	if !proc.Alive() {
		t.Fatal("front process was killed when only worker 1 should have been killed")
	}
	if proc.HasSession(s1) {
		t.Fatal("killed session s1 is still marked live")
	}
	if err := syscall.Kill(w2PID, 0); err != nil {
		t.Fatalf("healthy worker 2 (pid %d) died: %v", w2PID, err)
	}

	turn2 := proc.RunTurn(TurnSpec{SessionID: s2, Model: "gemini-3.8-flash-low", Prompt: []ContentBlock{TextBlock("ok")}})
	select {
	case <-turn2.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("healthy session s2 did not finish")
	}
	if out := turn2.Outcome(); out.Err != nil || out.StopReason != StopEndTurn {
		t.Fatalf("turn2 outcome = %+v, want StopEndTurn", out)
	}
}

func TestForceStopFallsBackToKillingProcessWhenFrontHangs(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	proc, err := StartProc(context.Background(), ProcConfig{
		Bin:       exe,
		Home:      dir,
		GeminiDir: filepath.Join(dir, "gemini"),
		Workdir:   dir,
		Env:       []string{envFrontIsolationStub + "=1", envFrontHangOnWorkerExit + "=1"},
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		proc.Kill()
		<-proc.Done()
	})

	s1, _, err := proc.NewSession(context.Background(), SessionSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	turn1 := proc.runTurn(TurnSpec{SessionID: s1, Model: "gemini-3.8-flash-low", Prompt: []ContentBlock{TextBlock("hang")}}, 40*time.Millisecond, 80*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	turn1.Cancel()

	select {
	case <-turn1.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not settle after fallback process kill")
	}
	select {
	case <-proc.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("hung front process was not killed after workerKillGrace")
	}
	if out := turn1.Outcome(); out.Err != nil || out.StopReason != StopCancelled {
		t.Fatalf("turn1 outcome = %+v, want StopCancelled", out)
	}
}
