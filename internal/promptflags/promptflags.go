// Package promptflags remembers the system prompts Google's risk control
// refused, in memory for the request path and in the store for good.
package promptflags

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agyrelay/internal/gateway"
	"agyrelay/internal/store"
)

// Flags implements gateway.PromptFlags.
type Flags struct {
	store *store.Store

	mu sync.RWMutex
	// until maps a fingerprint to the end of its flag.
	until map[string]time.Time
}

// New loads the flags still running.
func New(ctx context.Context, st *store.Store, log *slog.Logger) (*Flags, error) {
	active, err := st.ActiveFlaggedPrompts(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	f := &Flags{store: st, until: map[string]time.Time{}}
	for _, p := range active {
		f.until[p.Fingerprint] = p.Until
	}
	log.Info("flagged system prompts loaded", "active", len(active))
	return f, nil
}

func (f *Flags) FlaggedUntil(fingerprint string) time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.until[fingerprint]
}

// Flag refuses the prompt at once; the returned error says it was not
// saved, so a restart forgets it.
func (f *Flags) Flag(ctx context.Context, p gateway.FlaggedPrompt) error {
	f.mu.Lock()
	f.until[p.Fingerprint] = p.Until
	f.mu.Unlock()
	return f.store.FlagPrompt(ctx, store.FlaggedPrompt{
		Fingerprint: p.Fingerprint, Text: p.Text, LastAt: p.At, Until: p.Until,
		KeyHash: p.KeyHash, KeyName: p.KeyName, Account: p.Account, Model: p.Model,
	})
}

// List returns every flagged prompt without its text.
func (f *Flags) List(ctx context.Context) ([]store.FlaggedPrompt, error) {
	return f.store.FlaggedPrompts(ctx)
}

// Text returns the text of a flagged prompt.
func (f *Flags) Text(ctx context.Context, fingerprint string) (string, error) {
	return f.store.FlaggedPromptText(ctx, fingerprint)
}

// Cancel ends the prompt's flag now; the prompt stays on record. It
// returns store.ErrNotFound when no flag of the prompt is running.
func (f *Flags) Cancel(ctx context.Context, fingerprint string) error {
	if err := f.store.CancelFlaggedPrompt(ctx, fingerprint, time.Now()); err != nil {
		return err
	}
	f.mu.Lock()
	delete(f.until, fingerprint)
	f.mu.Unlock()
	return nil
}
