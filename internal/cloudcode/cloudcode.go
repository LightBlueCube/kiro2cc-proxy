// Package cloudcode is the loopback reverse proxy for agy's Cloud Code
// API. Each account's agy runs with --cloud_code_endpoint pointing at its
// own path prefix here; the proxy forwards to the real endpoint through
// the account's egress proxy, so routing and traffic stats stay in egress.
//
//	agy ──http──▶ 127.0.0.1:port/<account>/v1internal:… ──▶ egress ──▶ daily-cloudcode-pa.googleapis.com
//
// Only the model catalog (fetchAvailableModels) is rewritten: agy's
// checkpointer experiments are disabled and the window of models that have
// them raised, so agy neither summarizes ("auto-compacts") a long
// conversation nor drops its oldest history. Everything else, generation
// streams included, passes through unchanged.
package cloudcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultUpstream is the endpoint agy 1.5.3 uses without the flag.
const DefaultUpstream = "https://daily-cloudcode-pa.googleapis.com"

// Catalog rewrite, after realagy src/acp/catalog-proxy.ts.
const (
	catalogSuffix      = "/v1internal:fetchAvailableModels"
	checkpointerPrefix = "CASCADE_USE_EXPERIMENT_CHECKPOINTER"
	// disabledTokenLimit and disabledModelWindow keep a checkpointer that
	// ignores enabled=false from ever triggering.
	disabledTokenLimit  = "2147483647"
	disabledModelWindow = 1_000_000_000
	maxCatalogBytes     = 16 << 20
)

const (
	readHeaderTimeout   = 30 * time.Second
	idleConnTimeout     = 90 * time.Second
	tlsHandshakeTimeout = 30 * time.Second
)

// Server is the proxy shared by all accounts.
type Server struct {
	log      *slog.Logger
	upstream *url.URL
	listener net.Listener
	http     *http.Server

	mu       sync.Mutex
	accounts map[string]*accountProxy
}

type accountProxy struct {
	proxy     *httputil.ReverseProxy
	transport *http.Transport
}

// Start listens on a loopback port and forwards to upstream.
func Start(upstream string, log *slog.Logger) (*Server, error) {
	target, err := url.Parse(upstream)
	if err != nil || target.Scheme != "https" || target.Host == "" {
		return nil, fmt.Errorf("cloud code upstream %q: want an https:// URL", upstream)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cloud code proxy: %w", err)
	}
	s := &Server{log: log, upstream: target, listener: listener, accounts: map[string]*accountProxy{}}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		if err := s.http.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			log.Error("cloud code proxy stopped", "err", err)
		}
	}()
	return s, nil
}

// Close stops the proxy; requests in flight fail.
func (s *Server) Close() error { return s.http.Close() }

// Register routes the account's Cloud Code traffic through egressURL (its
// egress proxy URL with credentials) and returns the endpoint its agy
// must use.
func (s *Server) Register(name, egressURL string) (string, error) {
	egress, err := url.Parse(egressURL)
	if err != nil {
		return "", fmt.Errorf("egress URL for %s: %w", name, err)
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(egress),
		ForceAttemptHTTP2:   true,
		IdleConnTimeout:     idleConnTimeout,
		TLSHandshakeTimeout: tlsHandshakeTimeout,
	}
	prefix := "/" + name
	ap := &accountProxy{transport: transport}
	ap.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, prefix)
			r.Out.URL.RawPath = ""
			r.SetURL(s.upstream)
			if isCatalog(r.Out) {
				// The rewrite needs the body as plain JSON.
				r.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		Transport: transport,
		// Generation responses are server-sent events.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if !isCatalog(resp.Request) || resp.StatusCode != http.StatusOK {
				return nil
			}
			return s.rewriteCatalog(name, resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Error("cloud code request failed", "account", name, "path", r.URL.Path, "err", err)
			http.Error(w, "agyrelay: Cloud Code upstream request failed", http.StatusBadGateway)
		},
	}
	s.mu.Lock()
	old := s.accounts[name]
	s.accounts[name] = ap
	s.mu.Unlock()
	if old != nil {
		old.transport.CloseIdleConnections()
	}
	return "http://" + s.listener.Addr().String() + prefix, nil
}

// Forget stops routing the account.
func (s *Server) Forget(name string) {
	s.mu.Lock()
	ap := s.accounts[name]
	delete(s.accounts, name)
	s.mu.Unlock()
	if ap != nil {
		ap.transport.CloseIdleConnections()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	s.mu.Lock()
	ap := s.accounts[name]
	s.mu.Unlock()
	if ap == nil {
		http.Error(w, "agyrelay: unknown account", http.StatusNotFound)
		return
	}
	ap.proxy.ServeHTTP(w, r)
}

func isCatalog(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, catalogSuffix)
}

// rewriteCatalog replaces the catalog body with its rewritten form. An
// unreadable catalog fails the request: passing it through would let agy
// compact conversations.
func (s *Server) rewriteCatalog(account string, resp *http.Response) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read model catalog: %w", err)
	}
	if len(raw) > maxCatalogBytes {
		return fmt.Errorf("model catalog exceeds %d bytes", maxCatalogBytes)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return fmt.Errorf("model catalog arrived with Content-Encoding %q", encoding)
	}
	body, changed, err := rewriteCatalog(raw)
	if err != nil {
		return fmt.Errorf("rewrite model catalog: %w", err)
	}
	if changed == 0 {
		s.log.Warn("model catalog has no checkpointer configuration to disable", "account", account)
	} else {
		s.log.Debug("model catalog rewritten", "account", account, "checkpointer_configs", changed)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return nil
}

// rewriteCatalog disables every checkpointer experiment and raises
// maxTokens of the records carrying one. It returns the new body and the
// number of experiment configurations changed.
func rewriteCatalog(raw []byte) ([]byte, int, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, 0, err
	}
	changed := visit(payload)
	if changed == 0 {
		return raw, 0, nil
	}
	body, err := json.Marshal(payload)
	return body, changed, err
}

func visit(node any) int {
	changed := 0
	switch node := node.(type) {
	case []any:
		for _, v := range node {
			changed += visit(v)
		}
	case map[string]any:
		if hasCheckpointer(node["modelExperiments"]) {
			for _, key := range []string{"maxTokens", "max_tokens"} {
				if _, ok := node[key].(json.Number); ok {
					node[key] = json.Number(fmt.Sprint(disabledModelWindow))
				}
			}
		}
		for key, value := range node {
			if !strings.HasPrefix(key, checkpointerPrefix) {
				changed += visit(value)
				continue
			}
			if rewritten, ok := disableExperiment(value); ok {
				node[key] = rewritten
				changed++
			}
		}
	}
	return changed
}

func hasCheckpointer(modelExperiments any) bool {
	wrapper, _ := modelExperiments.(map[string]any)
	experiments, _ := wrapper["experiments"].(map[string]any)
	for key := range experiments {
		if strings.HasPrefix(key, checkpointerPrefix) {
			return true
		}
	}
	return false
}

// disableExperiment handles the shapes an experiment value takes: a JSON
// config object, that object encoded as a string, or a {stringValue}
// wrapper around the string.
func disableExperiment(value any) (any, bool) {
	switch value := value.(type) {
	case string:
		var config map[string]any
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		if decoder.Decode(&config) != nil || config == nil || !disableConfig(config) {
			return value, false
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			return value, false
		}
		return string(encoded), true
	case map[string]any:
		for _, key := range []string{"stringValue", "string_value"} {
			if inner, ok := value[key].(string); ok {
				rewritten, changed := disableExperiment(inner)
				value[key] = rewritten
				return value, changed
			}
		}
		return value, disableConfig(value)
	}
	return value, false
}

func disableConfig(config map[string]any) bool {
	changed := false
	if config["enabled"] != false {
		config["enabled"] = false
		changed = true
	}
	if config["max_token_limit"] != disabledTokenLimit {
		config["max_token_limit"] = disabledTokenLimit
		changed = true
	}
	return changed
}
