package control

import (
	"context"

	"agyrelay/internal/store"
)

// FlaggedPrompts lists the system prompts Google's risk control refused,
// without their text, the latest first.
func (c *Control) FlaggedPrompts(ctx context.Context) ([]store.FlaggedPrompt, error) {
	return c.cfg.Flags.List(ctx)
}

// FlaggedPromptText returns the text of a flagged system prompt.
func (c *Control) FlaggedPromptText(ctx context.Context, fingerprint string) (string, error) {
	return c.cfg.Flags.Text(ctx, fingerprint)
}

// CancelFlaggedPrompt ends a system prompt's flag now; requests with it
// go through again, and a new refusal flags it afresh.
func (c *Control) CancelFlaggedPrompt(ctx context.Context, fingerprint string) error {
	return c.cfg.Flags.Cancel(ctx, fingerprint)
}
