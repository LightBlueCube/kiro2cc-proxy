package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Protocol facts verified live against agy acp 1.5.x by realagy:
//
//   - clientInfo.name "zed" makes agy include Claude and GPT in its catalog;
//   - session/set_config_option takes configId "model" with the full slug
//     (effort tier baked in) and "mode";
//   - session/cancel is a notification; the prompt then ends "cancelled" and
//     the session stays usable;
//   - session/resume restores a session without replay and fails for unknown
//     ids; session/load replays history as notifications before answering and
//     silently returns an empty session for unknown ids;
//   - mcpServers entries are {name, type:"http", url, headers:[{name,value}]};
//   - agy exits when its stdin closes.

const (
	initTimeout      = 30 * time.Second
	sessionOpTimeout = 20 * time.Second
	killGrace        = 750 * time.Millisecond
	stderrTailBytes  = 8 << 10
)

// ErrAuthRequired means the account must sign in again.
var ErrAuthRequired = errors.New("antigravity sign-in required")

// ProcConfig describes one agy acp process.
type ProcConfig struct {
	Bin string
	// Home is the account's HOME; agy keeps credentials below it.
	Home string
	// GeminiDir is agy's data root (--gemini-dir).
	GeminiDir string
	// Workdir is the process working directory.
	Workdir string
	// Agent selects the custom agent profile (--agent).
	Agent string
	// CloudCodeEndpoint, when set, points agy at a loopback Cloud Code proxy.
	CloudCodeEndpoint string
	ProcessPerSession bool
	// Env is appended to the minimal environment (HOME, PATH, LANG).
	Env []string
	Log *slog.Logger
}

// MCPServer is an ACP mcpServers entry.
type MCPServer struct {
	Name    string       `json:"name"`
	Type    string       `json:"type"`
	URL     string       `json:"url"`
	Headers []HTTPHeader `json:"headers"`
}

type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SessionSpec carries what every session/new, resume, and load needs.
type SessionSpec struct {
	Cwd          string
	MCPServers   []MCPServer
	SystemAppend string
}

// ModelInfo is one entry of the ACP model catalog.
type ModelInfo struct {
	ModelID string `json:"modelId"`
	Name    string `json:"name,omitempty"`
}

// Usage is the turn-cumulative PromptResponse.usage.
type Usage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	ThoughtTokens     int64 `json:"thoughtTokens"`
	CachedReadTokens  int64 `json:"cachedReadTokens"`
	CachedWriteTokens int64 `json:"cachedWriteTokens"`
	TotalTokens       int64 `json:"totalTokens"`
}

// ContentBlock is an ACP prompt block.
type ContentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	Data     string    `json:"data,omitempty"`
	MimeType string    `json:"mimeType,omitempty"`
	Resource *Resource `json:"resource,omitempty"`
}

type Resource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

func TextBlock(text string) ContentBlock { return ContentBlock{Type: "text", Text: text} }

func ImageBlock(data, mimeType string) ContentBlock {
	return ContentBlock{Type: "image", Data: data, MimeType: mimeType}
}

// Proc is one running `agy acp` process.
type Proc struct {
	cfg   ProcConfig
	log   *slog.Logger
	cmd   *exec.Cmd
	stdin io.WriteCloser
	rpc   *rpcConn

	canResume bool
	version   string
	started   time.Time

	done    chan struct{}
	exitErr error
	stderr  tailBuffer

	mu         sync.Mutex
	listeners  map[string]func(json.RawMessage)
	suppressed map[string]bool
	live       map[string]bool
}

// StartProc spawns agy and completes the initialize handshake.
func StartProc(ctx context.Context, cfg ProcConfig) (*Proc, error) {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	args := []string{"acp", "--gemini-dir", cfg.GeminiDir, "--disable-telemetry", "--sandbox"}
	if cfg.Agent != "" {
		args = append(args, "--agent", cfg.Agent)
	}
	if cfg.CloudCodeEndpoint != "" {
		args = append(args, "--cloud_code_endpoint", cfg.CloudCodeEndpoint)
	}
	if cfg.ProcessPerSession {
		args = append(args, "--process-per-session")
	}

	cmd := exec.Command(cfg.Bin, args...)
	cmd.Dir = cfg.Workdir
	cmd.Env = append([]string{
		"HOME=" + cfg.Home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
		// The binary is pinned; agy must not replace itself.
		"AGY_CLI_DISABLE_AUTO_UPDATE=1",
	}, cfg.Env...)
	// Own process group: teardown also kills agy's helpers and workers.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("agy stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agy stdout: %w", err)
	}
	p := &Proc{
		cfg:        cfg,
		log:        log.With("agent", cfg.Agent),
		cmd:        cmd,
		stdin:      stdin,
		done:       make(chan struct{}),
		listeners:  map[string]func(json.RawMessage){},
		suppressed: map[string]bool{},
		live:       map[string]bool{},
	}
	p.stderr.limit = stderrTailBytes
	cmd.Stderr = &p.stderr
	p.rpc = newRPCConn(stdin, rpcHandlers{
		onRequest: p.onServerRequest,
		onNotify:  p.onNotification,
		onBadLine: func(line []byte) { p.log.Debug("agy stdout noise", "line", truncate(string(line), 200)) },
	})

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("run %s: %w", cfg.Bin, err)
	}
	p.started = time.Now()
	go p.wait(stdout)

	if err := p.initialize(ctx); err != nil {
		p.Kill()
		<-p.done
		return nil, err
	}
	return p, nil
}

func (p *Proc) wait(stdout io.Reader) {
	readErr := p.rpc.readLoop(stdout)
	waitErr := p.cmd.Wait()
	reason := describeExit(waitErr, readErr)
	if tail := lastLine(p.stderr.String()); tail != "" {
		reason = fmt.Sprintf("%s: %s", reason, tail)
	}
	p.exitErr = errors.New(reason)
	p.rpc.close(p.exitErr)
	p.log.Info("agy exited", "reason", reason)
	close(p.done)
}

func describeExit(waitErr, readErr error) string {
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return "agy " + exitErr.ProcessState.String()
	}
	if waitErr != nil {
		return "agy: " + waitErr.Error()
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "agy stdout: " + readErr.Error()
	}
	return "agy exited"
}

func (p *Proc) initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	var result struct {
		AgentInfo struct {
			Version string `json:"version"`
		} `json:"agentInfo"`
		AgentCapabilities struct {
			SessionCapabilities struct {
				Resume json.RawMessage `json:"resume"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	err := p.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		// Without a Zed-family clientInfo agy filters its catalog to Gemini.
		"clientInfo": map[string]string{"name": "zed", "title": "agyrelay", "version": "0.1.0"},
	}, &result)
	if err != nil {
		return fmt.Errorf("agy initialize: %w", err)
	}
	resume := result.AgentCapabilities.SessionCapabilities.Resume
	p.canResume = len(resume) > 0 && string(resume) != "null"
	p.version = result.AgentInfo.Version
	p.log.Info("agy initialized", "version", p.version, "resume", p.canResume)
	return nil
}

// Done is closed when the process has exited.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Err is the exit reason once Done is closed.
func (p *Proc) Err() error {
	select {
	case <-p.done:
		return p.exitErr
	default:
		return nil
	}
}

func (p *Proc) Alive() bool { return p.Err() == nil }

// PID is the process id (the front process under --process-per-session).
func (p *Proc) PID() int { return p.cmd.Process.Pid }

// Agent is the profile this process runs.
func (p *Proc) Agent() string { return p.cfg.Agent }

// Kill terminates the process group: SIGTERM, then SIGKILL after a grace.
func (p *Proc) Kill() {
	if p.cmd.Process == nil {
		return
	}
	pgid := -p.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	go func() {
		select {
		case <-p.done:
		case <-time.After(killGrace):
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}
	}()
}

// NewSession creates a session and returns its id and the model catalog.
func (p *Proc) NewSession(ctx context.Context, spec SessionSpec) (string, []ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	var result struct {
		SessionID string `json:"sessionId"`
		Models    struct {
			AvailableModels []ModelInfo `json:"availableModels"`
		} `json:"models"`
	}
	if err := p.call(ctx, "session/new", sessionParams("", spec), &result); err != nil {
		return "", nil, err
	}
	if result.SessionID == "" {
		return "", nil, errors.New("session/new returned no sessionId")
	}
	p.markLive(result.SessionID)
	return result.SessionID, result.Models.AvailableModels, nil
}

// HasSession reports whether this process holds the session in memory.
func (p *Proc) HasSession(sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live[sessionID]
}

// ReopenSession restores a persisted session: session/resume when agy
// advertises it, else session/load with its history replay dropped. Callers
// must check that the session's conversation store exists, because load of
// an unknown id yields an empty session.
func (p *Proc) ReopenSession(ctx context.Context, sessionID string, spec SessionSpec) error {
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	params := sessionParams(sessionID, spec)
	if p.canResume {
		if err := p.call(ctx, "session/resume", params, nil); err != nil {
			return err
		}
		p.markLive(sessionID)
		return nil
	}
	p.setSuppressed(sessionID, true)
	defer p.setSuppressed(sessionID, false)
	if err := p.call(ctx, "session/load", params, nil); err != nil {
		return err
	}
	p.markLive(sessionID)
	return nil
}

func sessionParams(sessionID string, spec SessionSpec) map[string]any {
	servers := spec.MCPServers
	if servers == nil {
		servers = []MCPServer{}
	}
	params := map[string]any{"cwd": spec.Cwd, "mcpServers": servers}
	if sessionID != "" {
		params["sessionId"] = sessionID
	}
	antigravity := map[string]any{"eagerMcpServers": []string{BridgeServer}}
	if spec.SystemAppend != "" {
		antigravity["systemPrompt"] = map[string]string{"append": spec.SystemAppend}
	}
	params["_meta"] = map[string]any{"antigravity": antigravity}
	return params
}

// SetConfigOption sets a session option ("model", "mode").
func (p *Proc) SetConfigOption(ctx context.Context, sessionID, configID, value string) error {
	ctx, cancel := context.WithTimeout(ctx, sessionOpTimeout)
	defer cancel()
	return p.call(ctx, "session/set_config_option", map[string]string{
		"sessionId": sessionID, "configId": configID, "value": value,
	}, nil)
}

type promptResult struct {
	StopReason string `json:"stopReason"`
	Usage      *Usage `json:"usage"`
}

// prompt runs session/prompt until it ends. There is no request timeout:
// the turn's inactivity guard governs it.
func (p *Proc) prompt(ctx context.Context, sessionID string, blocks []ContentBlock) (promptResult, error) {
	var result promptResult
	err := p.call(ctx, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": blocks}, &result)
	if err != nil {
		return result, err
	}
	if result.StopReason == "" {
		return result, errors.New("session/prompt returned no stopReason")
	}
	return result, nil
}

// cancel asks agy to end the session's running prompt.
func (p *Proc) cancel(sessionID string) {
	if err := p.rpc.Notify("session/cancel", map[string]string{"sessionId": sessionID}); err != nil {
		p.log.Warn("session/cancel failed", "session", sessionID, "err", err)
	}
}

func (p *Proc) call(ctx context.Context, method string, params, out any) error {
	err := p.rpc.Call(ctx, method, params, out)
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) && rpcErr.Code == codeAuthRequired {
		return fmt.Errorf("%w (%s: %s)", ErrAuthRequired, method, rpcErr.Message)
	}
	return err
}

// subscribe routes the session's live updates to fn until the returned
// function is called.
func (p *Proc) subscribe(sessionID string, fn func(json.RawMessage)) func() {
	p.mu.Lock()
	p.listeners[sessionID] = fn
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.listeners, sessionID)
		p.mu.Unlock()
	}
}

func (p *Proc) markLive(sessionID string) {
	p.mu.Lock()
	p.live[sessionID] = true
	p.mu.Unlock()
}

func (p *Proc) setSuppressed(sessionID string, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		p.suppressed[sessionID] = true
		return
	}
	delete(p.suppressed, sessionID)
}

func (p *Proc) onNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var frame struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &frame) != nil {
		return
	}
	p.mu.Lock()
	fn := p.listeners[frame.SessionID]
	suppressed := p.suppressed[frame.SessionID]
	p.mu.Unlock()
	if fn == nil || suppressed {
		return // load replay, or no turn running on this session
	}
	fn(frame.Update)
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

// onServerRequest answers agy's requests. Permission requests are denied:
// under the profile agy has no native tool worth approving, and approving
// would let a model act on this server.
func (p *Proc) onServerRequest(method string, params json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, fmt.Errorf("client capability not enabled: %s", method)
	}
	var req struct {
		Options []permissionOption `json:"options"`
	}
	_ = json.Unmarshal(params, &req)
	for _, option := range req.Options {
		if strings.HasPrefix(option.Kind, "reject") {
			p.log.Warn("denied agy permission request", "option", option.OptionID)
			return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": option.OptionID}}, nil
		}
	}
	return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
}

// tailBuffer keeps the last limit bytes written.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return truncate(strings.TrimSpace(lines[len(lines)-1]), 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
