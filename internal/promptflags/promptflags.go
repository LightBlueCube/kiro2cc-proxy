// Package promptflags remembers the system prompts Google's risk control
// refused, in memory for the request path and in the store.
package promptflags

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"agyrelay/internal/gateway"
	"agyrelay/internal/store"
)

// Flags implements gateway.PromptFlags.
type Flags struct {
	store   *store.Store
	enabled atomic.Bool

	mu sync.RWMutex
	// flagged holds the prompts being refused, by fingerprint.
	flagged map[string]flag
}

type flag struct {
	// until is the end of the flag; a pinned prompt is refused after it.
	until  time.Time
	pinned bool
}

// New loads the flags still running. Flags start enabled.
func New(ctx context.Context, st *store.Store, log *slog.Logger) (*Flags, error) {
	active, err := st.ActiveFlaggedPrompts(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	f := &Flags{store: st, flagged: map[string]flag{}}
	f.enabled.Store(true)
	for _, p := range active {
		f.flagged[p.Fingerprint] = flag{until: p.Until, pinned: p.Pinned}
	}
	log.Info("flagged system prompts loaded", "active", len(active))
	return f, nil
}

// SetEnabled turns the refusal of flagged prompts on or off; the flags
// stay on record while it is off.
func (f *Flags) SetEnabled(enabled bool) { f.enabled.Store(enabled) }

// Enabled is whether flagged prompts are refused.
func (f *Flags) Enabled() bool { return f.enabled.Load() }

func (f *Flags) Flagged(fingerprint string) (until time.Time, pinned bool) {
	if !f.enabled.Load() {
		return time.Time{}, false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	fl := f.flagged[fingerprint]
	return fl.until, fl.pinned
}

// Flag refuses the prompt at once; the returned error says it was not
// saved, so a restart forgets it. A pinned prompt stays pinned.
func (f *Flags) Flag(ctx context.Context, p gateway.FlaggedPrompt) error {
	f.mu.Lock()
	fl := f.flagged[p.Fingerprint]
	fl.until = p.Until
	f.flagged[p.Fingerprint] = fl
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

// Cancel ends the prompt's flag now and unpins it; the prompt stays on
// record. It returns store.ErrNotFound when the prompt is neither pinned
// nor flagged.
func (f *Flags) Cancel(ctx context.Context, fingerprint string) error {
	if err := f.store.CancelFlaggedPrompt(ctx, fingerprint, time.Now()); err != nil {
		return err
	}
	f.mu.Lock()
	delete(f.flagged, fingerprint)
	f.mu.Unlock()
	return nil
}

// Pin keeps the prompt refused until it is unpinned. It returns
// store.ErrNotFound when there is no such prompt on record.
func (f *Flags) Pin(ctx context.Context, fingerprint string) error {
	if err := f.store.PinFlaggedPrompt(ctx, fingerprint); err != nil {
		return err
	}
	f.mu.Lock()
	fl := f.flagged[fingerprint]
	fl.pinned = true
	f.flagged[fingerprint] = fl
	f.mu.Unlock()
	return nil
}

// Unpin lets the prompt's flag run out, and restarts its retention now.
// It returns store.ErrNotFound when the prompt is not pinned.
func (f *Flags) Unpin(ctx context.Context, fingerprint string) error {
	now := time.Now()
	if err := f.store.UnpinFlaggedPrompt(ctx, fingerprint, now); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fl := f.flagged[fingerprint]
	fl.pinned = false
	if now.Before(fl.until) {
		f.flagged[fingerprint] = fl
		return nil
	}
	delete(f.flagged, fingerprint)
	return nil
}
