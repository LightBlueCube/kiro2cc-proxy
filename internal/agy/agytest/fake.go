// Package agytest provides a scripted stand-in for `agy acp`, run by test
// binaries in place of the real agy.
//
// The live input of each prompt (text after the last </history>, or the
// whole last text block) may hold one directive line:
//
//	@tool NAME JSON      call the client tool NAME, reply "tool said: <result>";
//	                     a result "@hang" makes it produce nothing until cancelled
//	@parallel A B ...    call tools A, B, ... through the parallel tool
//	@hang                produce nothing until cancelled
//	@think-hang          a thought, then nothing until cancelled
//	@steps               reply "steps: N", N the steps before this prompt
//
// Anything else gets a thought and "reply to: <input>".
//
// With --login the fake signs in like `agy acp --login`: it prints a
// Google link and accepts LoginCode.
//
// Its language server also answers GetUserStatus and
// RetrieveUserQuotaSummary; the gemini-weekly bucket loses
// QuotaPerPrompt for every prompt the process served.
//
// Like agy, the fake runs a language server with the session's steps
// (stored in its conversation file) and logs its port. Each model call
// step reports Step* tokens, and a turn's usage sums them. After
// RevertToCascadeStep the next prompt replies "stale stream" unless the
// session was resumed, as agy streams stale text then. Environment:
//
//	FAKE_AGY_LOG    append every request as a JSON line; language server
//	                calls have the method "ls/<Name>"
//	FAKE_AGY_QUOTA  answer every prompt with agy's quota notice
//	FAKE_AGY_MODELS report these comma-separated model ids instead of
//	                Models
package agytest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Step types the fake records, as agy names them.
const (
	stepUserInput = "CORTEX_STEP_TYPE_USER_INPUT"
	stepPlanner   = "CORTEX_STEP_TYPE_PLANNER_RESPONSE"
	stepGeneric   = "CORTEX_STEP_TYPE_GENERIC"
	// stepErrorMessage records a failed model call.
	stepErrorMessage = "CORTEX_STEP_TYPE_ERROR_MESSAGE"
)

// Tokens of each model call (PLANNER_RESPONSE step): uncached input,
// cache reads, and output, which includes the thoughts.
const (
	StepInput   = 60
	StepCached  = 40
	StepOutput  = 10
	StepThought = 5
)

// stepModelUsage is a step's metadata.modelUsage, in agy's format.
var stepModelUsage = map[string]string{
	"inputTokens": fmt.Sprint(StepInput), "cacheReadTokens": fmt.Sprint(StepCached),
	"outputTokens": fmt.Sprint(StepOutput), "thinkingOutputTokens": fmt.Sprint(StepThought),
}

// OverflowError is the model error of the "@overflow" prompt, as agy
// records it for Gemini.
const OverflowError = "INVALID_ARGUMENT (code 400): The input token count exceeds the maximum number of tokens allowed 1048576."

// EnvRun makes a test binary act as the fake agent (see Main).
const EnvRun = "FAKE_AGY"

// QuotaNotice is agy's exhausted-quota reply.
const QuotaNotice = "Usage Limit Reached\n\nYou have reached your current quota for this period. Your limit will reset in 2 hours."

// LoginCode is the authorization code the fake login accepts; it signs in
// LoginEmail.
const (
	LoginCode  = "4/fake-code"
	LoginEmail = "fake@example.com"
)

// QuotaPerPrompt is the gemini-weekly share each prompt uses.
const QuotaPerPrompt = 0.01

func login() {
	fmt.Println("Open this link in a browser to log in with Google:")
	fmt.Println()
	fmt.Println("  https://accounts.google.com/o/oauth2/auth?client_id=fake&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback")
	fmt.Println()
	fmt.Println("Then paste the authorization code, or the full URL of the page you land on, and press Enter:")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != LoginCode {
		fmt.Println(`couldn't log in: token exchange failed: oauth2: "invalid_grant" "Bad Request"`)
		os.Exit(1)
	}
	fmt.Printf("Logged in as %s.\n", LoginEmail)
}

// RefreshToken is the imported refresh token the fake accepts.
const RefreshToken = "1//fake-refresh-token"

// signedIn mirrors agy's check of an imported sign-in: with a token file,
// session/new needs its refresh token and auth_method "consumer". Without
// one the fake counts as signed in, as before imports existed.
func (f *fake) signedIn() bool {
	raw, err := os.ReadFile(filepath.Join(f.geminiDir, "antigravity-cli", "antigravity-oauth-token"))
	if err != nil {
		return true
	}
	var record struct {
		Token struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
		AuthMethod string `json:"auth_method"`
	}
	return json.Unmarshal(raw, &record) == nil && record.Token.RefreshToken == RefreshToken && record.AuthMethod == "consumer"
}

// Models is the catalog the fake reports.
var Models = []map[string]string{
	{"modelId": "gemini-3.8-flash-low", "name": "Gemini 3.8 Flash (Low)"},
	{"modelId": "gemini-3.8-flash-high", "name": "Gemini 3.8 Flash (High)"},
	{"modelId": "claude-opus-5-5-high", "name": "Claude Opus 5.5 (High)"},
}

// catalog is Models, or the ids FAKE_AGY_MODELS lists.
func catalog() []map[string]string {
	ids := os.Getenv("FAKE_AGY_MODELS")
	if ids == "" {
		return Models
	}
	var out []map[string]string
	for _, id := range strings.Split(ids, ",") {
		out = append(out, map[string]string{"modelId": id, "name": id})
	}
	return out
}

type fake struct {
	geminiDir string
	started   string // log file timestamp
	log       *os.File
	lsOnce    sync.Once

	outMu sync.Mutex
	out   *bufio.Writer

	mu       sync.Mutex
	sessions map[string]*fakeSession
	nextID   int
	prompts  int
}

type fakeSession struct {
	url, token string
	cancel     context.CancelFunc
	steps      []string
	stale      bool
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Main serves ACP on stdin/stdout until stdin closes.
func Main() {
	f := &fake{out: bufio.NewWriter(os.Stdout), sessions: map[string]*fakeSession{}, started: time.Now().Format("20060102_150405")}
	for i, arg := range os.Args {
		if arg == "--gemini-dir" && i+1 < len(os.Args) {
			f.geminiDir = os.Args[i+1]
		}
	}
	if slices.Contains(os.Args, "--login") {
		login()
		return
	}
	// The fake is its own only worker (agy acp --process-per-session).
	f.appendLog("acp-front", fmt.Sprintf("front: started worker 1 (pid %d)", os.Getpid()))
	if path := os.Getenv("FAKE_AGY_LOG"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake agy:", err)
			os.Exit(1)
		}
		f.log = file
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		var msg rpcMessage
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Method == "" {
			continue // responses to our requests are not used
		}
		f.record(scanner.Bytes())
		go f.handle(msg)
	}
}

func (f *fake) record(line []byte) {
	if f.log == nil {
		return
	}
	f.outMu.Lock()
	defer f.outMu.Unlock()
	f.log.Write(append(append([]byte{}, line...), '\n'))
}

func (f *fake) send(v any) {
	raw, _ := json.Marshal(v)
	f.outMu.Lock()
	defer f.outMu.Unlock()
	f.out.Write(raw)
	f.out.WriteByte('\n')
	f.out.Flush()
}

func (f *fake) reply(id json.RawMessage, result any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fake) fail(id json.RawMessage, code int, message string) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (f *fake) update(sessionID string, update map[string]any) {
	f.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": update}})
}

func (f *fake) chunk(sessionID, kind, text string) {
	f.update(sessionID, map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}})
}

type sessionParams struct {
	SessionID  string `json:"sessionId"`
	MCPServers []struct {
		URL     string `json:"url"`
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"mcpServers"`
}

func (f *fake) handle(msg rpcMessage) {
	switch msg.Method {
	case "initialize":
		f.reply(msg.ID, map[string]any{
			"protocolVersion":   1,
			"agentInfo":         map[string]string{"name": "fake-agy", "version": "1.5.3"},
			"agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}},
		})
	case "session/new":
		if !f.signedIn() {
			f.fail(msg.ID, -32000, "Authentication required")
			return
		}
		var p sessionParams
		json.Unmarshal(msg.Params, &p)
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("fake-%d-%d", os.Getpid(), f.nextID)
		f.mu.Unlock()
		os.MkdirAll(filepath.Dir(f.store(id)), 0o700)
		f.open(id, p, nil)
		f.reply(msg.ID, map[string]any{"sessionId": id, "models": map[string]any{"availableModels": catalog()}})
	case "session/resume", "session/load":
		var p sessionParams
		json.Unmarshal(msg.Params, &p)
		raw, err := os.ReadFile(f.store(p.SessionID))
		if err != nil {
			f.fail(msg.ID, -32602, "unknown session")
			return
		}
		var steps []string
		json.Unmarshal(raw, &steps)
		f.open(p.SessionID, p, steps)
		f.reply(msg.ID, map[string]any{})
	case "session/close":
		var p sessionParams
		json.Unmarshal(msg.Params, &p)
		f.mu.Lock()
		delete(f.sessions, p.SessionID)
		f.mu.Unlock()
		f.reply(msg.ID, map[string]any{})
	case "session/set_config_option":
		var p struct {
			ConfigID string `json:"configId"`
			Value    string `json:"value"`
		}
		json.Unmarshal(msg.Params, &p)
		offered := slices.ContainsFunc(catalog(), func(m map[string]string) bool { return m["modelId"] == p.Value })
		if p.ConfigID == "model" && !offered {
			// agy 1.5.3's answer to a slug outside the account's catalog.
			f.fail(msg.ID, -32602, "Unknown model ID: "+p.Value)
			return
		}
		f.reply(msg.ID, map[string]any{"configOptions": []any{}})
	case "session/prompt":
		f.prompt(msg)
	case "session/cancel":
		var p sessionParams
		json.Unmarshal(msg.Params, &p)
		f.mu.Lock()
		if s := f.sessions[p.SessionID]; s != nil && s.cancel != nil {
			s.cancel()
		}
		f.mu.Unlock()
	default:
		if msg.ID != nil {
			f.fail(msg.ID, -32601, "method not found")
		}
	}
}

func (f *fake) store(id string) string {
	return filepath.Join(f.geminiDir, "antigravity-cli", "conversations", id+".db")
}

func (f *fake) open(id string, p sessionParams, steps []string) {
	f.lsOnce.Do(f.startLanguageServer)
	s := &fakeSession{steps: steps}
	if len(p.MCPServers) > 0 {
		s.url = p.MCPServers[0].URL
		for _, h := range p.MCPServers[0].Headers {
			s.token = h.Value
		}
	}
	f.mu.Lock()
	f.sessions[id] = s
	f.saveLocked(id, s)
	f.mu.Unlock()
	f.appendLog("acp-front", fmt.Sprintf("front: session %s served by worker 1", id))
}

// addSteps appends steps to the session's trajectory.
func (f *fake) addSteps(id string, s *fakeSession, steps ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.steps = append(s.steps, steps...)
	f.saveLocked(id, s)
}

func (f *fake) saveLocked(id string, s *fakeSession) {
	raw, _ := json.Marshal(s.steps)
	os.WriteFile(f.store(id), raw, 0o600)
}

// appendLog writes a glog-style line to <kind>-<ts>-<pid>.log.
func (f *fake) appendLog(kind, line string) {
	dir := filepath.Join(f.geminiDir, "antigravity-cli", "log")
	os.MkdirAll(dir, 0o700)
	file, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%s-%s-%d.log", kind, f.started, os.Getpid())), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake agy:", err)
		os.Exit(1)
	}
	defer file.Close()
	fmt.Fprintf(file, "I%s       1 fake.go:1] %s\n", time.Now().Format("0102 15:04:05.000000"), line)
}

func (f *fake) startLanguageServer() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake agy:", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exa.language_server_pb.LanguageServerService/{method}", f.serveLanguageServer)
	go http.Serve(listener, mux)
	f.appendLog("acp-worker", fmt.Sprintf("Language server listening on random port at %d for HTTP", listener.Addr().(*net.TCPAddr).Port))
}

func (f *fake) serveLanguageServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CascadeID        string `json:"cascadeId"`
		StepOffset       int    `json:"stepOffset"`
		StepIndex        int    `json:"stepIndex"`
		ConversationOnly bool   `json:"conversationOnly"`
	}
	var raw []byte
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	logged, _ := json.Marshal(map[string]any{"method": "ls/" + r.PathValue("method"), "params": req})
	f.record(logged)

	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.PathValue("method") {
	case "GetUserStatus":
		writeJSON(w, map[string]any{"userStatus": map[string]any{
			"email": LoginEmail, "name": "Fake User",
			"planStatus": map[string]any{"planInfo": map[string]any{"planName": "Pro"}},
			"userTier":   map[string]any{"name": "Google AI Pro"},
		}})
		return
	case "RetrieveUserQuotaSummary":
		reset := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
		writeJSON(w, map[string]any{"response": map[string]any{"groups": []any{
			map[string]any{"displayName": "Gemini Models", "buckets": []any{
				map[string]any{"bucketId": "gemini-weekly", "displayName": "Weekly Limit Remaining", "window": "weekly", "remainingFraction": 1 - QuotaPerPrompt*float64(f.prompts), "resetTime": reset},
				map[string]any{"bucketId": "gemini-5h", "displayName": "Five Hour Limit Remaining", "window": "5h", "remainingFraction": 1, "resetTime": reset},
			}},
			map[string]any{"displayName": "Claude and GPT models", "buckets": []any{
				map[string]any{"bucketId": "3p-weekly", "displayName": "Weekly Limit Remaining", "window": "weekly", "remainingFraction": 1, "resetTime": reset},
			}},
		}}})
		return
	}
	s := f.sessions[req.CascadeID]
	if s == nil {
		http.Error(w, "run state not found", http.StatusInternalServerError)
		return
	}
	switch r.PathValue("method") {
	case "GetCascadeTrajectorySteps":
		steps := []map[string]any{}
		execution := ""
		for i, step := range s.steps {
			// Like agy, each prompt's steps share an executionId.
			if step == stepUserInput {
				execution = fmt.Sprintf("exec-%d", i)
			}
			if i < req.StepOffset {
				continue
			}
			metadata := map[string]any{"executionId": execution}
			entry := map[string]any{"type": step, "metadata": metadata}
			switch step {
			case stepPlanner:
				metadata["modelUsage"] = stepModelUsage
			case stepErrorMessage:
				entry["errorMessage"] = map[string]any{"error": map[string]any{"modelErrorMessage": OverflowError, "errorCode": 400}}
			}
			steps = append(steps, entry)
		}
		raw, _ = json.Marshal(map[string]any{"steps": steps})
	case "RevertToCascadeStep":
		s.steps = s.steps[:min(req.StepIndex+1, len(s.steps))]
		s.stale = true
		f.saveLocked(req.CascadeID, s)
		raw, _ = json.Marshal(map[string]any{"metadata": map[string]any{}})
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}

func writeJSON(w http.ResponseWriter, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}

func (f *fake) prompt(msg rpcMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	json.Unmarshal(msg.Params, &p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.mu.Lock()
	s := f.sessions[p.SessionID]
	if s == nil {
		f.mu.Unlock()
		f.fail(msg.ID, -32602, "unknown session")
		return
	}
	s.cancel = cancel
	f.mu.Unlock()

	input := ""
	for _, block := range p.Prompt {
		if block.Type == "text" {
			input = block.Text
		}
	}
	f.chunk(p.SessionID, "user_message_chunk", input)
	if i := strings.LastIndex(input, "</history>"); i >= 0 {
		input = input[i+len("</history>"):]
	}
	input = strings.TrimSpace(input)

	f.mu.Lock()
	stepsBefore, stale := len(s.steps), s.stale
	f.prompts++
	f.mu.Unlock()
	f.addSteps(p.SessionID, s, stepUserInput)
	if input == "@overflow" {
		// agy's reply when the model call fails; the cause is in the step.
		f.addSteps(p.SessionID, s, stepErrorMessage)
		f.chunk(p.SessionID, "agent_message_chunk", "\n⚠ Agent execution terminated due to error.\n")
		f.reply(msg.ID, map[string]any{"stopReason": "end_turn"})
		return
	}
	text, err := f.respond(ctx, s, p.SessionID, input)
	if ctx.Err() != nil {
		f.reply(msg.ID, map[string]any{"stopReason": "cancelled"})
		return
	}
	if err != nil {
		f.fail(msg.ID, -32603, err.Error())
		return
	}
	if input == "@steps" {
		text = fmt.Sprintf("steps: %d", stepsBefore)
	}
	if stale {
		text = "stale stream"
	}
	f.addSteps(p.SessionID, s, stepPlanner)
	f.chunk(p.SessionID, "agent_message_chunk", text)

	// Like agy's, the turn's usage sums its model calls.
	f.mu.Lock()
	calls := int64(0)
	for _, step := range s.steps[stepsBefore:] {
		if step == stepPlanner {
			calls++
		}
	}
	f.mu.Unlock()
	f.reply(msg.ID, map[string]any{"stopReason": "end_turn", "usage": map[string]int64{
		"inputTokens": calls * StepInput, "cachedReadTokens": calls * StepCached, "outputTokens": calls * StepOutput,
		"thoughtTokens": calls * StepThought, "totalTokens": calls * (StepInput + StepCached + StepOutput)}})
}

func (f *fake) respond(ctx context.Context, s *fakeSession, sessionID, input string) (string, error) {
	if os.Getenv("FAKE_AGY_QUOTA") != "" {
		return QuotaNotice, nil
	}
	fields := strings.Fields(input)
	directive := ""
	if len(fields) > 0 {
		directive = fields[0]
	}
	switch directive {
	case "@hang":
		<-ctx.Done()
		return "", nil
	case "@think-hang":
		f.chunk(sessionID, "agent_thought_chunk", "thinking")
		<-ctx.Done()
		return "", nil
	case "@tool":
		name, args, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(input, "@tool")), " ")
		f.addSteps(sessionID, s, stepPlanner)
		result, err := callTool(ctx, s, name, json.RawMessage(args))
		if err != nil {
			return "", err
		}
		f.addSteps(sessionID, s, stepGeneric)
		if result == "@hang" {
			<-ctx.Done()
			return "", nil
		}
		return "tool said: " + result, nil
	case "@parallel":
		var calls []map[string]any
		for _, name := range fields[1:] {
			calls = append(calls, map[string]any{"name": name, "arguments": map[string]any{}})
		}
		args, _ := json.Marshal(map[string]any{"calls": calls})
		f.addSteps(sessionID, s, stepPlanner)
		result, err := callTool(ctx, s, "parallel", args)
		if err != nil {
			return "", err
		}
		f.addSteps(sessionID, s, stepGeneric)
		return "tool said: " + result, nil
	}
	f.chunk(sessionID, "agent_thought_chunk", "thinking")
	return "reply to: " + input, nil
}

type tokenTransport struct{ token string }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Relay-Token", t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func callTool(ctx context.Context, s *fakeSession, name string, args json.RawMessage) (string, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-agy", Version: "1.5.3"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             s.url,
		HTTPClient:           &http.Client{Transport: tokenTransport{s.token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("connect bridge: %w", err)
	}
	defer session.Close()
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("call %s: %w", name, err)
	}
	var texts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}
