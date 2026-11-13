package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agyrelay/internal/agy"
	"agyrelay/internal/store"
)

// googleRefreshPrefix starts every Google OAuth refresh token; access
// tokens start with accessTokenPrefix.
const (
	googleRefreshPrefix = "1//"
	accessTokenPrefix   = "ya29."
)

// ImportSignIn signs the account in with a pasted refresh token, or a
// token JSON holding one, instead of the Google sign-in, and returns the
// signed-in email. A quota refresh checks the token; when it fails, the
// previous sign-in is put back.
func (c *Control) ImportSignIn(ctx context.Context, name, pasted string) (string, error) {
	account := c.cfg.Pool.Account(name)
	if account == nil {
		return "", store.ErrNotFound
	}
	if err := account.Blocked(); err != nil {
		return "", &InputError{"cannot import: " + err.Error()}
	}
	refreshToken, err := refreshTokenFrom(pasted)
	if err != nil {
		return "", &InputError{err.Error()}
	}

	c.CancelLogin(name)
	undo, err := agy.ImportSignIn(account.GeminiDir(), refreshToken)
	if err != nil {
		return "", err
	}
	// agy reads the sign-in when it starts.
	account.Restart()
	q, err := c.cfg.Monitor.Refresh(ctx, account)
	if err != nil {
		account.Restart() // its agy holds the rejected token
		if undoErr := undo(); undoErr != nil {
			return "", fmt.Errorf("the token did not sign in (%v), and restoring the previous sign-in failed: %w", err, undoErr)
		}
		return "", &InputError{"the token did not sign in: " + err.Error()}
	}
	account.SignedIn()
	return q.User.Email, nil
}

// refreshTokenFrom finds the refresh token in what the admin pasted: the
// token itself, or JSON with one refresh_token (or refreshToken) value at
// any depth, as agy's own token file and other tools' exports have it.
func refreshTokenFrom(pasted string) (string, error) {
	token := strings.TrimSpace(pasted)
	if strings.HasPrefix(token, "{") || strings.HasPrefix(token, "[") {
		var doc any
		if err := json.Unmarshal([]byte(token), &doc); err != nil {
			return "", fmt.Errorf("unreadable JSON: %v", err)
		}
		found := map[string]bool{}
		collectRefreshTokens(doc, found)
		switch len(found) {
		case 0:
			return "", errors.New("the JSON has no refresh_token")
		case 1:
			for t := range found {
				token = t
			}
		default:
			return "", fmt.Errorf("the JSON holds %d refresh tokens; paste one account's", len(found))
		}
	}

	switch {
	case strings.HasPrefix(token, accessTokenPrefix):
		return "", errors.New("this is an access token; paste the refresh token (it starts with 1//)")
	case !strings.HasPrefix(token, googleRefreshPrefix) || strings.ContainsAny(token, " \t\r\n\""):
		return "", errors.New("not a Google refresh token (it starts with 1//)")
	}
	return token, nil
}

func collectRefreshTokens(v any, found map[string]bool) {
	switch v := v.(type) {
	case map[string]any:
		for key, value := range v {
			if s, ok := value.(string); ok && (key == "refresh_token" || key == "refreshToken") {
				found[s] = true
				continue
			}
			collectRefreshTokens(value, found)
		}
	case []any:
		for _, value := range v {
			collectRefreshTokens(value, found)
		}
	}
}
