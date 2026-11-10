package agy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Newline-delimited JSON-RPC 2.0 over agy's stdio.
//
//   - requests are correlated by numeric id;
//   - server-to-client requests (session/request_permission, fs/*, …) carry
//     both method and id and are answered through onRequest;
//   - malformed lines are logged and dropped: foreign processes sometimes
//     write into agy's inherited stdout (e.g. a browser launcher banner);
//   - a line longer than maxFrameBytes kills the connection.

const maxFrameBytes = 32 << 20

// Lines foreign processes are known to print into agy's stdout.
var noiseMarkers = []string{"Opening in existing browser session."}

// RPCError is a JSON-RPC error response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

// ACP auth_required.
const codeAuthRequired = -32000

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcHandlers struct {
	// onRequest answers a server-to-client request.
	onRequest func(method string, params json.RawMessage) (any, error)
	onNotify  func(method string, params json.RawMessage)
	onBadLine func(line []byte)
}

type rpcConn struct {
	handlers rpcHandlers

	writeMu sync.Mutex
	w       io.Writer

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResult
	closed  bool
	reason  error
}

func newRPCConn(w io.Writer, handlers rpcHandlers) *rpcConn {
	return &rpcConn{w: w, handlers: handlers, pending: map[int64]chan rpcResult{}}
}

// readLoop parses frames until r fails, then closes the connection with
// the read error. It returns that error.
func (c *rpcConn) readLoop(r io.Reader) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := readFrame(br)
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			c.close(err)
			return err
		}
	}
}

// readFrame returns one line without its newline, refusing lines over
// maxFrameBytes.
func readFrame(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxFrameBytes {
			return nil, fmt.Errorf("stdout frame exceeds %d bytes", maxFrameBytes)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSpace(buf), err
	}
}

func (c *rpcConn) handleLine(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		repaired, ok := stripNoise(line)
		if !ok || json.Unmarshal(repaired, &msg) != nil {
			if c.handlers.onBadLine != nil {
				c.handlers.onBadLine(line)
			}
			return
		}
	}

	hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
	switch {
	case msg.Method == "" && hasID:
		c.deliver(msg)
	case msg.Method != "" && hasID:
		go c.answer(msg)
	case msg.Method != "":
		if c.handlers.onNotify != nil {
			c.handlers.onNotify(msg.Method, msg.Params)
		}
	}
}

// stripNoise repairs a frame that a noise fragment without newline glued
// itself onto. A pure noise line yields an empty, ignorable result.
func stripNoise(line []byte) ([]byte, bool) {
	for _, marker := range noiseMarkers {
		if idx := bytes.Index(line, []byte(marker)); idx >= 0 {
			return bytes.TrimSpace(line[idx+len(marker):]), true
		}
	}
	return nil, false
}

func (c *rpcConn) deliver(msg rpcMessage) {
	var id int64
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return // late answer to a request whose caller gave up
	}
	if msg.Error != nil {
		ch <- rpcResult{err: msg.Error}
		return
	}
	ch <- rpcResult{result: msg.Result}
}

func (c *rpcConn) answer(msg rpcMessage) {
	reply := rpcMessage{JSONRPC: "2.0", ID: msg.ID}
	if c.handlers.onRequest == nil {
		reply.Error = &RPCError{Code: -32601, Message: "method not supported: " + msg.Method}
		_ = c.write(reply)
		return
	}
	result, err := c.handlers.onRequest(msg.Method, msg.Params)
	if err != nil {
		reply.Error = &RPCError{Code: -32601, Message: err.Error()}
		_ = c.write(reply)
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		reply.Error = &RPCError{Code: -32603, Message: err.Error()}
		_ = c.write(reply)
		return
	}
	reply.Result = raw
	_ = c.write(reply)
}

// Call sends a request and decodes its result into out (when non-nil).
// Cancelling ctx abandons the wait; agy may still act on the request.
func (c *rpcConn) Call(ctx context.Context, method string, params, out any) error {
	ch := make(chan rpcResult, 1)
	c.mu.Lock()
	if c.closed {
		reason := c.reason
		c.mu.Unlock()
		return fmt.Errorf("%s: connection closed: %w", method, reason)
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	rawID, _ := json.Marshal(id)
	rawParams, err := json.Marshal(params)
	if err != nil {
		c.forget(id)
		return fmt.Errorf("%s: encode params: %w", method, err)
	}
	if err := c.write(rpcMessage{JSONRPC: "2.0", ID: rawID, Method: method, Params: rawParams}); err != nil {
		c.forget(id)
		return fmt.Errorf("%s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if out == nil || len(res.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(res.result, out); err != nil {
			return fmt.Errorf("%s: decode result: %w", method, err)
		}
		return nil
	}
}

// Notify sends a notification (no id, no answer).
func (c *rpcConn) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("%s: encode params: %w", method, err)
	}
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *rpcConn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *rpcConn) write(msg rpcMessage) error {
	frame, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	frame = append(frame, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(frame)
	return err
}

// close fails every pending call with reason; later calls fail at once.
func (c *rpcConn) close(reason error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.reason = reason
	pending := c.pending
	c.pending = map[int64]chan rpcResult{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcResult{err: fmt.Errorf("connection closed: %w", reason)}
	}
}
