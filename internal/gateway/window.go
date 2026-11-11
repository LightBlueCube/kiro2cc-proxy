package gateway

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"

	"agyrelay/internal/conv"
	"agyrelay/internal/tokens"
)

// ContextWindow caps the prompt of a model, in tokens as that model
// counts them. A request whose estimated prompt is larger fails with
// context_length_exceeded before it reaches an account.
type ContextWindow struct {
	Model  string `json:"model"`
	Tokens int    `json:"tokens"`
}

// DefaultContextWindows are the upstream limits, so overflows fail here
// without spending quota. Gemini's 1048576 and gpt-oss's 131072 come from
// their overflow errors; agy reserves 32768 output tokens on gpt-oss
// (maxOutputTokens), leaving 98304 for the prompt. Claude's 1000000 is the
// catalog's maxTokens (unverified as a prompt limit).
var DefaultContextWindows = []ContextWindow{
	{Model: "gemini-4-argon", Tokens: 1048576},
	{Model: "gemini-3.8-flash", Tokens: 1048576},
	{Model: "claude-opus-5-5", Tokens: 1000000},
	{Model: "claude-sonnet-5-5", Tokens: 1000000},
	{Model: "gpt-oss-120b", Tokens: 98304},
}

const maxContextWindow = 100_000_000

// ValidateContextWindows reports the first malformed entry.
func ValidateContextWindows(windows []ContextWindow) error {
	seen := map[string]bool{}
	for _, w := range windows {
		if strings.TrimSpace(w.Model) == "" {
			return errors.New("every context window needs a model id")
		}
		if w.Tokens < 1 || w.Tokens > maxContextWindow {
			return fmt.Errorf("context window of %s must be between 1 and %d tokens", w.Model, maxContextWindow)
		}
		if seen[w.Model] {
			return fmt.Errorf("%s has two context windows", w.Model)
		}
		seen[w.Model] = true
	}
	return nil
}

func windowMap(windows []ContextWindow) map[string]int {
	m := make(map[string]int, len(windows))
	for _, w := range windows {
		m[w.Model] = w.Tokens
	}
	return m
}

// agy's prompt around the client's, measured on fresh requests (gpt-oss /
// Gemini 3.8 Flash / Claude Sonnet):
//
//	no client tools                557 / 568 / 1074
//	one small tool (glob)          +337 / +516 / +848
//	each further tool              its name, description, and schema
//	                               counted, plus 38 / 87 / 115 on average
//	                               over 7 tools; at most 99 / 86 / 115 for
//	                               single tools with 12 properties or
//	                               nested arrays
//
// The first tool brings the parallel tool, the tool policy, and Claude's
// tool-use preamble.
const (
	agyPromptTokens       = 1100
	toolsetTokens         = 700
	toolDeclarationTokens = 130
	// countCacheEntries bounds each generation of the count cache.
	countCacheEntries = 50_000
)

// promptEstimator estimates the prompt a request costs upstream. Clients
// resend the whole conversation, so counts are cached by text.
type promptEstimator struct {
	tokens *tokens.Set

	mu       sync.Mutex
	cur, old map[countKey]int
}

type countKey struct {
	counter string
	text    [sha256.Size]byte
}

func newPromptEstimator(set *tokens.Set) *promptEstimator {
	return &promptEstimator{tokens: set, cur: map[countKey]int{}, old: map[countKey]int{}}
}

// estimate counts what replaying the request would send: agy's prompt,
// the client's system text, every message as replay renders it, the tool
// declarations, and every image (replay drops older ones, a warm session
// keeps them).
func (e *promptEstimator) estimate(modelID string, req conv.Request, t *transcript) (int, error) {
	counter := e.tokens.For(modelID)
	texts := make([]string, 0, len(t.body)+len(req.Tools)+1)
	if t.system != "" {
		texts = append(texts, "<client-system>\n"+t.system+"\n</client-system>")
	}
	total := agyPromptTokens
	for _, m := range t.body {
		texts = append(texts, renderMessage(m, -1))
		for _, image := range m.Images {
			total += counter.Image(image.Data)
		}
	}
	if len(req.Tools) > 0 {
		total += toolsetTokens
	}
	for _, tool := range req.Tools {
		texts = append(texts, tool.Name+"\n"+tool.Description+"\n"+string(tool.Parameters))
		total += toolDeclarationTokens
	}
	for _, text := range texts {
		n, err := e.count(counter, text)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (e *promptEstimator) count(counter tokens.Counter, text string) (int, error) {
	key := countKey{counter: counter.Name(), text: sha256.Sum256([]byte(text))}
	if n, ok := e.lookup(key); ok {
		return n, nil
	}
	n, err := counter.Count(text)
	if err != nil {
		return 0, err
	}
	e.store(key, n)
	return n, nil
}

func (e *promptEstimator) lookup(key countKey) (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n, ok := e.cur[key]; ok {
		return n, true
	}
	n, ok := e.old[key]
	if ok {
		e.storeLocked(key, n)
	}
	return n, ok
}

func (e *promptEstimator) store(key countKey, n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.storeLocked(key, n)
}

// storeLocked adds to the current generation; a full one becomes the old
// generation, dropping the entries not used since the last swap.
func (e *promptEstimator) storeLocked(key countKey, n int) {
	if len(e.cur) >= countCacheEntries {
		e.old, e.cur = e.cur, map[countKey]int{}
	}
	e.cur[key] = n
}
