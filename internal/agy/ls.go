package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// agy's language server holds each session's trajectory ("cascade", whose
// id is the ACP sessionId) as a list of steps. Facts verified against agy
// 1.5.3 with a signed-in account:
//
//   - The server starts with the process's first session. Its log
//     <gemini-dir>/antigravity-cli/log/acp-server-<ts>-<pid>.log has the line
//     "Language server listening on random port at <port> for HTTP".
//   - With --process-per-session each worker runs its own server and logs to
//     acp-worker-<ts>-<workerPid>.log; the front's acp-front-<ts>-<pid>.log
//     has "started worker N (pid P)" and "session <id> served by worker N".
//   - Calls are unauthenticated JSON POSTs to lsServicePath + method.
//   - GetCascadeTrajectorySteps{cascadeId, stepOffset} lists the steps from
//     stepOffset on. A finished turn ends with a PLANNER_RESPONSE step.
//   - RevertToCascadeStep{cascadeId, stepIndex, conversationOnly} keeps steps
//     0..stepIndex. It fails for sessions the process has not loaded. A
//     prompt that follows it streams stale text until session/resume.

const (
	lsServicePath = "/exa.language_server_pb.LanguageServerService/"
	lsCallTimeout = 10 * time.Second
)

var (
	lsListenLine  = regexp.MustCompile(`(?m)Language server listening on random port at (\d+) for HTTP$`)
	startedWorker = regexp.MustCompile(`started worker (\d+) \(pid (\d+)\)`)
)

// StepCount returns the session's native step count. known must not exceed
// it; only the steps after known are fetched.
func (p *Proc) StepCount(ctx context.Context, sessionID string, known int) (int, error) {
	var result struct {
		Steps []json.RawMessage `json:"steps"`
	}
	err := p.lsCall(ctx, sessionID, "GetCascadeTrajectorySteps", map[string]any{"cascadeId": sessionID, "stepOffset": known}, &result)
	if err != nil {
		return 0, err
	}
	return known + len(result.Steps), nil
}

// Rewind keeps the session's first keep steps (keep ≥ 1) and resumes it, so
// the next prompt streams correctly. The session must be live in p.
func (p *Proc) Rewind(ctx context.Context, sessionID string, keep int, spec SessionSpec) error {
	if !p.canResume {
		return errors.New("rewind needs session/resume, which this agy does not offer")
	}
	params := map[string]any{"cascadeId": sessionID, "stepIndex": keep - 1, "conversationOnly": true}
	if err := p.lsCall(ctx, sessionID, "RevertToCascadeStep", params, &struct{}{}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	return p.call(ctx, "session/resume", sessionParams(sessionID, spec), nil)
}

func (p *Proc) lsCall(ctx context.Context, sessionID, method string, params, out any) error {
	port, err := p.lsPort(sessionID)
	if err != nil {
		return fmt.Errorf("find language server of session %s: %w", sessionID, err)
	}
	return lsPost(ctx, port, method, params, out)
}

// lsPost calls method on the language server at port.
func lsPost(ctx context.Context, port, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lsCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+lsServicePath+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("language server %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("language server %s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("language server %s: %s: %s", method, resp.Status, truncate(string(raw), 300))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("language server %s: %w", method, err)
	}
	return nil
}

// errNoLanguageServer means the process has not started one yet: agy
// starts it with the first session.
var errNoLanguageServer = errors.New("no language server yet")

// accountCall calls an account-level method on any of the process's
// language servers, newest first; under --process-per-session workers
// retire, so earlier ones may be gone.
func (p *Proc) accountCall(ctx context.Context, method string, out any) error {
	ports, err := p.lsPorts()
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return errNoLanguageServer
	}
	var errs []error
	for _, port := range ports {
		err := lsPost(ctx, port, method, map[string]any{}, out)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// lsPorts lists the process's language server ports, newest first.
func (p *Proc) lsPorts() ([]string, error) {
	pid := p.cmd.Process.Pid
	if !p.cfg.ProcessPerSession {
		port, err := p.listenPort("acp-server", pid)
		if err != nil {
			return nil, nil // not started yet
		}
		return []string{port}, nil
	}
	front, err := p.readLog("acp-front", pid)
	if err != nil {
		return nil, err
	}
	var ports []string
	started := startedWorker.FindAllStringSubmatch(front, -1)
	for i := len(started) - 1; i >= 0; i-- {
		workerPid, _ := strconv.Atoi(started[i][2])
		if port, err := p.listenPort("acp-worker", workerPid); err == nil {
			ports = append(ports, port)
		}
	}
	return ports, nil
}

// lsPort finds the HTTP port of the language server holding the session.
// It reads the logs on every call: a worker may have been replaced.
func (p *Proc) lsPort(sessionID string) (string, error) {
	pid := p.cmd.Process.Pid
	if !p.cfg.ProcessPerSession {
		return p.listenPort("acp-server", pid)
	}
	front, err := p.readLog("acp-front", pid)
	if err != nil {
		return "", err
	}
	servedBy := regexp.MustCompile(`session ` + regexp.QuoteMeta(sessionID) + ` served by worker (\d+)`)
	served := servedBy.FindAllStringSubmatch(front, -1)
	if served == nil {
		return "", errors.New("no worker serves it")
	}
	worker := served[len(served)-1][1]
	workerPid := 0
	for _, m := range startedWorker.FindAllStringSubmatch(front, -1) {
		if m[1] == worker {
			workerPid, _ = strconv.Atoi(m[2])
		}
	}
	if workerPid == 0 {
		return "", fmt.Errorf("worker %s has no logged pid", worker)
	}
	return p.listenPort("acp-worker", workerPid)
}

func (p *Proc) listenPort(kind string, pid int) (string, error) {
	text, err := p.readLog(kind, pid)
	if err != nil {
		return "", err
	}
	m := lsListenLine.FindStringSubmatch(text)
	if m == nil {
		return "", fmt.Errorf("%s log of pid %d names no port", kind, pid)
	}
	return m[1], nil
}

// readLog reads the newest <kind>-<ts>-<pid>.log; pids may repeat over time.
func (p *Proc) readLog(kind string, pid int) (string, error) {
	pattern := filepath.Join(p.cfg.GeminiDir, "antigravity-cli", "log", fmt.Sprintf("%s-*-%d.log", kind, pid))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no %s log for pid %d", kind, pid)
	}
	raw, err := os.ReadFile(matches[len(matches)-1])
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
