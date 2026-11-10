// Package bridge serves client tools to agy over a loopback MCP Streamable
// HTTP endpoint. It only transports calls: every call is handed to the
// binding's Handler, which decides how and when it is answered.
package bridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TokenHeader carries a binding's shared secret.
const TokenHeader = "X-Relay-Token"

const protocolVersionHeader = "Mcp-Protocol-Version"

// Versions the SDK speaks. agy may send a header version the SDK does not
// know; the stateless transport cannot remember a negotiated version, so
// such headers are clamped to the SDK's latest.
var supportedProtocolVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is a client tool as MCP lists it.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object.
	InputSchema json.RawMessage
}

// Image is base64 image data.
type Image struct {
	Data     string
	MimeType string
}

// Result answers one tool call.
type Result struct {
	Text    string
	Images  []Image
	IsError bool
}

// Handler answers the tool calls of one binding. Call blocks until the
// result is known; ctx ends when agy drops the HTTP request.
type Handler interface {
	Call(ctx context.Context, name string, args json.RawMessage) (Result, error)
}

// Server is the loopback MCP endpoint shared by all bindings.
type Server struct {
	log      *slog.Logger
	listener net.Listener
	http     *http.Server
	base     string

	mu       sync.Mutex
	bindings map[string]*Binding
}

// Binding is one session's tool catalog and its call handler.
type Binding struct {
	server *Server
	id     string
	token  string
	mcp    *mcp.Server
}

type bindingKey struct{}

// Start listens on a random loopback port.
func Start(log *slog.Logger) (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bridge listen: %w", err)
	}
	s := &Server{
		log:      log,
		listener: listener,
		base:     "http://" + listener.Addr().String(),
		bindings: map[string]*Binding{},
	}
	sdk := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		binding, _ := r.Context().Value(bindingKey{}).(*Binding)
		return binding.mcp
	}, &mcp.StreamableHTTPOptions{Stateless: true})
	s.http = &http.Server{Handler: s.authorize(sdk)}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("bridge stopped", "err", err)
		}
	}()
	return s, nil
}

// Close stops the endpoint; outstanding calls see their context end.
func (s *Server) Close() error { return s.http.Close() }

// authorize resolves /mcp/<id> to its binding and checks the token.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		binding := s.bindings[id]
		s.mu.Unlock()
		if binding == nil {
			http.NotFound(w, r)
			return
		}
		token := r.Header.Get(TokenHeader)
		if subtle.ConstantTimeCompare([]byte(token), []byte(binding.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if v := r.Header.Get(protocolVersionHeader); v != "" && !slices.Contains(supportedProtocolVersions, v) {
			r.Header.Set(protocolVersionHeader, supportedProtocolVersions[0])
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bindingKey{}, binding)))
	})
}

// Bind registers a tool catalog. agy keeps the tool list a session started
// with, so a binding lives as long as its session is in use.
func (s *Server) Bind(tools []Tool, handler Handler) (*Binding, error) {
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	token, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agyrelay", Version: "0.1.0"}, nil)
	for _, tool := range tools {
		server.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		}, toolHandler(tool.Name, handler))
	}
	b := &Binding{server: s, id: id, token: token, mcp: server}
	s.mu.Lock()
	s.bindings[id] = b
	s.mu.Unlock()
	return b, nil
}

// URL is the binding's MCP endpoint.
func (b *Binding) URL() string { return b.server.base + "/mcp/" + b.id }

// Token is the value agy must send in TokenHeader.
func (b *Binding) Token() string { return b.token }

// Close unregisters the binding; later calls get 404.
func (b *Binding) Close() {
	b.server.mu.Lock()
	delete(b.server.bindings, b.id)
	b.server.mu.Unlock()
}

func toolHandler(name string, handler Handler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.Params.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		result, err := handler.Call(ctx, name, args)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: " + err.Error()}},
				IsError: true,
			}, nil
		}
		return toMCPResult(result), nil
	}
}

func toMCPResult(result Result) *mcp.CallToolResult {
	text := result.Text
	if text == "" {
		text = "(no output)"
		if len(result.Images) > 0 {
			text = "(image result)"
		}
	}
	content := []mcp.Content{&mcp.TextContent{Text: text}}
	for _, image := range result.Images {
		data, err := base64.StdEncoding.DecodeString(image.Data)
		if err != nil {
			content = append(content, &mcp.TextContent{Text: "[image could not be decoded]"})
			continue
		}
		content = append(content, &mcp.ImageContent{Data: data, MIMEType: image.MimeType})
	}
	return &mcp.CallToolResult{Content: content, IsError: result.IsError}
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
