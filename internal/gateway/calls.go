package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"agyrelay/internal/bridge"
	"agyrelay/internal/conv"
)

var errTurnEnded = errors.New("the Antigravity turn ended before the client returned this tool result")

// pendingCall is one client tool call shown to the client as a tool_call.
type pendingCall struct {
	id   string
	name string
	args json.RawMessage
}

// callGroup is one MCP tools/call from agy: a single client tool, or a
// parallel batch expanded into several client tool calls.
//
//	agy ──tools/call──▶ callGroup ──Inject──▶ response: tool_calls
//	agy ◀──result───── settle ◀──── next request: tool messages
type callGroup struct {
	id       string
	calls    []*pendingCall
	parallel bool

	once   sync.Once
	done   chan struct{}
	result bridge.Result
	err    error
}

func (c *callGroup) settle(result bridge.Result, err error) {
	c.once.Do(func() {
		c.result, c.err = result, err
		close(c.done)
	})
}

func (c *callGroup) toolCalls() []conv.ToolCall {
	out := make([]conv.ToolCall, len(c.calls))
	for i, call := range c.calls {
		out[i] = conv.ToolCall{ID: call.id, Name: call.name, Arguments: call.args}
	}
	return out
}

func newCallID(prefix string) string {
	return prefix + strings.ToLower(rand.Text()[:20])
}

// newGroup builds the group for one MCP call. A malformed parallel call is
// answered at once with an error result for the model.
func newGroup(name string, args json.RawMessage, known map[string]bool) (*callGroup, *bridge.Result) {
	if name != parallelToolName {
		return &callGroup{
			id:    newCallID("call_"),
			calls: []*pendingCall{{id: newCallID("call_"), name: name, args: args}},
			done:  make(chan struct{}),
		}, nil
	}
	calls, err := parseParallel(args, func(n string) bool { return known[n] })
	if err != nil {
		return nil, &bridge.Result{Text: "Error: " + err.Error(), IsError: true}
	}
	group := &callGroup{id: newCallID("batch_"), parallel: true, done: make(chan struct{})}
	for _, call := range calls {
		group.calls = append(group.calls, &pendingCall{id: newCallID("call_"), name: call.Name, args: call.Arguments})
	}
	return group, nil
}

// groupResult answers a group from the client's tool messages. User
// messages the client added after the results ride on the last result.
func groupResult(group *callGroup, results map[string]conv.Message, trailing []conv.Message) bridge.Result {
	parts := make([]bridge.Result, len(group.calls))
	for i, call := range group.calls {
		msg := results[call.id]
		var steering []conv.Message
		if i == len(group.calls)-1 {
			steering = trailing
		}
		parts[i] = bridge.Result{Text: toolResultText(msg, steering), Images: bridgeImages(msg.Images)}
	}
	if !group.parallel {
		return parts[0]
	}
	return formatParallel(group.calls, parts)
}

func bridgeImages(images []conv.Image) []bridge.Image {
	out := make([]bridge.Image, len(images))
	for i, image := range images {
		out[i] = bridge.Image{Data: image.Data, MimeType: image.MimeType}
	}
	return out
}

// matchResults checks that delta answers exactly the group's calls, then
// holds only user messages. OpenAI clients return every result of a
// tool_calls message before anything else.
func matchResults(group *callGroup, delta []conv.Message) (map[string]conv.Message, []conv.Message, bool) {
	if len(delta) < len(group.calls) {
		return nil, nil, false
	}
	want := map[string]bool{}
	for _, call := range group.calls {
		want[call.id] = true
	}
	results := map[string]conv.Message{}
	for _, m := range delta[:len(group.calls)] {
		if m.Role != conv.RoleTool || !want[m.ToolCallID] {
			return nil, nil, false
		}
		if _, dup := results[m.ToolCallID]; dup {
			return nil, nil, false
		}
		results[m.ToolCallID] = m
	}
	trailing := delta[len(group.calls):]
	for _, m := range trailing {
		if m.Role != conv.RoleUser {
			return nil, nil, false
		}
	}
	return results, trailing, true
}

// sessionTools answers the bridge calls of one session.
type sessionTools struct {
	g    *Gateway
	sess *session
}

func (h sessionTools) Call(ctx context.Context, name string, args json.RawMessage) (bridge.Result, error) {
	g := h.g
	g.mu.Lock()
	at := h.sess.turn
	if at == nil || at.turn == nil || at.finished {
		g.mu.Unlock()
		return bridge.Result{}, errors.New("no Antigravity turn is running for this session")
	}
	group, rejected := newGroup(name, args, h.sess.known)
	if rejected != nil {
		g.mu.Unlock()
		return *rejected, nil
	}
	at.groups[group.id] = group
	// Under g.mu so the group is registered before a consumer sees it.
	at.turn.Inject(group)
	g.mu.Unlock()
	return await(ctx, at, group)
}

// await blocks an MCP call until the client answers the group or the turn
// ends; parkTTL ends a turn whose client never answers, well inside the
// patched agy's 24 h MCP timeout. The turn's inactivity guard pauses
// meanwhile: the model produces nothing while it waits for a tool.
func await(ctx context.Context, at *activeTurn, group *callGroup) (bridge.Result, error) {
	at.turn.Park()
	defer at.turn.Unpark()
	select {
	case <-group.done:
		return group.result, group.err
	case <-ctx.Done():
		return bridge.Result{}, ctx.Err()
	}
}
