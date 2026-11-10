// Package server authenticates API keys and routes OpenAI endpoints.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"agyrelay/internal/conv"
	"agyrelay/internal/openai"
)

// Key is one API key, stored as its SHA-256 so the config holds no secret.
type Key struct {
	Name   string
	SHA256 [sha256.Size]byte
}

type Server struct {
	keys   []Key
	openai *openai.Handler
	mux    *http.ServeMux
}

func New(keys []Key, handler *openai.Handler) *Server {
	s := &Server{keys: keys, openai: handler, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if user, ok := s.authenticate(w, r); ok {
			s.openai.Chat(w, r, user)
		}
	})
	s.mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if user, ok := s.authenticate(w, r); ok {
			s.openai.Responses(w, r, user)
		}
	})
	s.mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.authenticate(w, r); ok {
			s.openai.Models(w, r)
		}
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// authenticate resolves the bearer token (or x-api-key) to a key name.
// Every configured key is compared so timing does not reveal which matched.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		token = r.Header.Get("X-Api-Key")
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	user := ""
	for _, key := range s.keys {
		if subtle.ConstantTimeCompare(sum[:], key.SHA256[:]) == 1 {
			user = key.Name
		}
	}
	if token == "" || user == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		openai.WriteError(w, &conv.Error{Kind: conv.ErrUnauthorized, Message: "invalid API key"})
		return "", false
	}
	return user, true
}
