package control

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"agyrelay/internal/agy"
	"agyrelay/internal/store"
)

// loginTTL ends a sign-in nobody finished.
const loginTTL = 15 * time.Minute

type pendingLogin struct {
	login *agy.Login
	timer *time.Timer
}

// StartLogin begins a Google sign-in for the account and returns the link
// to open. A sign-in already pending for it is replaced.
func (c *Control) StartLogin(ctx context.Context, id string) (string, error) {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return "", store.ErrNotFound
	}
	if err := account.Blocked(); err != nil {
		return "", &InputError{"cannot sign in: " + err.Error()}
	}
	c.CancelLogin(id)
	login, err := agy.StartLogin(ctx, agy.LoginConfig{
		Bin:       c.cfg.AgyBin,
		Home:      account.Home(),
		GeminiDir: filepath.Join(account.Home(), ".gemini"),
		Workdir:   account.Workspace(),
		Env:       account.Env(),
	})
	if err != nil {
		return "", err
	}
	pending := &pendingLogin{login: login}
	pending.timer = time.AfterFunc(loginTTL, func() { c.endLogin(id, pending) })
	c.mu.Lock()
	c.logins[id] = pending
	c.mu.Unlock()
	return login.URL(), nil
}

// SubmitLogin finishes the account's pending sign-in with the code (or the
// landing page URL) and returns the signed-in email. On success the
// account's agy restarts with the new sign-in and its quota is sampled.
func (c *Control) SubmitLogin(ctx context.Context, id, code string) (string, error) {
	c.mu.Lock()
	pending := c.logins[id]
	c.mu.Unlock()
	account := c.cfg.Pool.Account(id)
	if pending == nil || account == nil {
		return "", &InputError{"no sign-in is pending for this account; start one first"}
	}
	email, err := pending.login.Submit(ctx, code)
	c.endLogin(id, pending)
	if err != nil {
		return "", &InputError{fmt.Sprintf("sign-in failed: %v", err)}
	}
	// What Google said about the old sign-in no longer holds.
	c.cfg.CloudCode.ClearVerification(id)
	account.SyncCooldowns(nil, time.Time{})
	account.SignedIn()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := c.cfg.Monitor.Refresh(ctx, account); err != nil {
			c.log.Warn("first quota sample after sign-in failed", "account", account.Name(), "err", err)
		}
	}()
	return email, nil
}

// CancelLogin ends the account's pending sign-in, if any.
func (c *Control) CancelLogin(id string) {
	c.mu.Lock()
	pending := c.logins[id]
	c.mu.Unlock()
	if pending != nil {
		c.endLogin(id, pending)
	}
}

// LoginPending reports whether a sign-in waits for its code.
func (c *Control) LoginPending(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.logins[id] != nil
}

func (c *Control) endLogin(id string, pending *pendingLogin) {
	pending.timer.Stop()
	pending.login.Close()
	c.mu.Lock()
	if c.logins[id] == pending {
		delete(c.logins, id)
	}
	c.mu.Unlock()
}
