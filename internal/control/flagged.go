package control

import (
	"context"

	"agyrelay/internal/store"
)

// FlaggedPrompts lists the system prompts Google's risk control refused,
// without their bodies, the latest first.
func (c *Control) FlaggedPrompts(ctx context.Context) ([]store.FlaggedPrompt, error) {
	return c.cfg.Flags.List(ctx)
}

// FlaggedPrompt returns one flagged prompt with its text and captured
// upstream call.
func (c *Control) FlaggedPrompt(ctx context.Context, fingerprint string) (store.FlaggedPrompt, error) {
	return c.cfg.Flags.Get(ctx, fingerprint)
}

// CancelFlaggedPrompt ends a system prompt's flag now and unpins it;
// requests with it go through again, and a new refusal flags it afresh.
func (c *Control) CancelFlaggedPrompt(ctx context.Context, fingerprint string) error {
	return c.cfg.Flags.Cancel(ctx, fingerprint)
}

// PinFlaggedPrompt keeps a system prompt refused, and on record, until it
// is unpinned.
func (c *Control) PinFlaggedPrompt(ctx context.Context, fingerprint string) error {
	return c.cfg.Flags.Pin(ctx, fingerprint)
}

// UnpinFlaggedPrompt lets a pinned prompt's flag run out and restarts its
// retention.
func (c *Control) UnpinFlaggedPrompt(ctx context.Context, fingerprint string) error {
	return c.cfg.Flags.Unpin(ctx, fingerprint)
}

// DeleteFlaggedPrompt removes a flagged prompt record and lifts any active
// flag or pin on it.
func (c *Control) DeleteFlaggedPrompt(ctx context.Context, fingerprint string) error {
	return c.cfg.Flags.Delete(ctx, fingerprint)
}
