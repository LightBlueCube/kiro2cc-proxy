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
func (c *Control) StartLogin(ctx context.Context, name string) (string, error) {
	account := c.cfg.Pool.Account(name)
	if account == nil {
		return "", store.ErrNotFound
	}
	if err := account.Blocked(); err != nil {
		return "", &InputError{"cannot sign in: " + err.Error()}
	}
	c.CancelLogin(name)
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
	pending.timer = time.AfterFunc(loginTTL, func() { c.endLogin(name, pending) })
	c.mu.Lock()
	c.logins[name] = pending
	c.mu.Unlock()
	return login.URL(), nil
}

// SubmitLogin finishes the account's pending sign-in with the code (or the
// landing page URL) and returns the signed-in email. On success the
// account's agy restarts with the new sign-in and its quota is sampled.
func (c *Control) SubmitLogin(ctx context.Context, name, code string) (string, error) {
	c.mu.Lock()
	pending := c.logins[name]
	c.mu.Unlock()
	account := c.cfg.Pool.Account(name)
	if pending == nil || account == nil {
		return "", &InputError{"no sign-in is pending for this account; start one first"}
	}
	email, err := pending.login.Submit(ctx, code)
	c.endLogin(name, pending)
	if err != nil {
		return "", &InputError{fmt.Sprintf("sign-in failed: %v", err)}
	}
	// What Google said about the old sign-in no longer holds.
	c.cfg.CloudCode.ClearVerification(name)
	account.SignedIn()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := c.cfg.Monitor.Refresh(ctx, account); err != nil {
			c.log.Warn("first quota sample after sign-in failed", "account", name, "err", err)
		}
	}()
	return email, nil
}

// CancelLogin ends the account's pending sign-in, if any.
func (c *Control) CancelLogin(name string) {
	c.mu.Lock()
	pending := c.logins[name]
	c.mu.Unlock()
	if pending != nil {
		c.endLogin(name, pending)
	}
}

// LoginPending reports whether a sign-in waits for its code.
func (c *Control) LoginPending(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.logins[name] != nil
}

func (c *Control) endLogin(name string, pending *pendingLogin) {
	pending.timer.Stop()
	pending.login.Close()
	c.mu.Lock()
	if c.logins[name] == pending {
		delete(c.logins, name)
	}
	c.mu.Unlock()
}
