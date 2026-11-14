// Package server authenticates API keys and routes the OpenAI endpoints
// and the admin UI.
package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agyrelay/internal/conv"
	"agyrelay/internal/guard"
	"agyrelay/internal/openai"
)

// Keys resolves API keys.
type Keys interface {
	Lookup(token string) (conv.Caller, bool)
}

type Config struct {
	Keys   Keys
	OpenAI *openai.Handler
	// Admin serves /admin/; nil leaves the admin UI off.
	Admin http.Handler
	// Proxies may name the client of a request.
	Proxies guard.Proxies
}

type Server struct {
	keys    Keys
	openai  *openai.Handler
	limiter *guard.Limiter
	proxies guard.Proxies
	mux     *http.ServeMux
}

// Bad API keys: a client gets 20 tries per 10 minutes, everyone together
// 500. Keys carry 130 random bits, so this only keeps scanners cheap.
var apiKeyLimits = guard.Config{PerClient: 20, Global: 500, Window: 10 * time.Minute, Lockout: 10 * time.Minute, MaxLockout: 24 * time.Hour}

func New(cfg Config) *Server {
	s := &Server{keys: cfg.Keys, openai: cfg.OpenAI, limiter: guard.NewLimiter(apiKeyLimits), proxies: cfg.Proxies, mux: http.NewServeMux()}
	handle := func(pattern string, fn func(http.ResponseWriter, *http.Request, conv.Caller)) {
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if caller, ok := s.authenticate(w, r); ok {
				fn(w, r, caller)
			}
		})
	}
	handle("POST /v1/chat/completions", s.openai.Chat)
	handle("POST /v1/responses", s.openai.Responses)
	handle("GET /v1/models", s.openai.Models)
	handle("GET /v1/dashboard/billing/subscription", s.openai.BillingSubscription)
	handle("GET /dashboard/billing/subscription", s.openai.BillingSubscription)
	handle("GET /v1/dashboard/billing/usage", s.openai.BillingUsage)
	handle("GET /dashboard/billing/usage", s.openai.BillingUsage)
	s.mux.HandleFunc("/{$}", func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	if cfg.Admin != nil {
		s.mux.Handle("/admin/", cfg.Admin)
		s.mux.Handle("/user/", cfg.Admin)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// authenticate resolves the bearer token (or x-api-key) to its caller.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (conv.Caller, bool) {
	addr := s.proxies.ClientIP(r)
	if ok, wait := s.limiter.Allow(addr); !ok {
		seconds := int(wait.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		openai.WriteError(w, &conv.Error{Kind: conv.ErrQuota, Message: fmt.Sprintf("too many invalid API keys; retry in %d s", seconds)})
		return conv.Caller{}, false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		token = r.Header.Get("X-Api-Key")
	}
	caller, ok := s.keys.Lookup(strings.TrimSpace(token))
	if !ok {
		s.limiter.Fail(addr)
		w.Header().Set("WWW-Authenticate", "Bearer")
		openai.WriteError(w, &conv.Error{Kind: conv.ErrUnauthorized, Message: "invalid API key"})
		return conv.Caller{}, false
	}
	return caller, true
}
