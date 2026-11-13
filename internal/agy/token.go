package agy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// tokenFile is where agy 1.5.3 keeps its sign-in below the gemini dir. It
// uses the file because no D-Bus secret service runs for it.
const tokenFile = "antigravity-cli/antigravity-oauth-token"

// consumerAuth marks a personal Google account. agy reads a lone refresh
// token only with it: without it session/new answers "Authentication
// required" (verified 1.5.3).
const consumerAuth = "consumer"

// ImportSignIn replaces the sign-in in geminiDir with a refresh token of
// agy's OAuth client. agy refreshes it at its next start and fills in the
// rest of the record. undo puts the previous sign-in back.
func ImportSignIn(geminiDir, refreshToken string) (undo func() error, err error) {
	path := filepath.Join(geminiDir, tokenFile)
	previous, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read sign-in: %w", err)
	}
	record, err := json.Marshal(map[string]any{
		"token":       map[string]string{"refresh_token": refreshToken},
		"auth_method": consumerAuth,
	})
	if err != nil {
		return nil, err
	}
	if err := writePrivate(path, record); err != nil {
		return nil, fmt.Errorf("write sign-in: %w", err)
	}
	undo = func() error {
		if existed {
			return writePrivate(path, previous)
		}
		return os.Remove(path)
	}
	return undo, nil
}

// writePrivate replaces path atomically with an owner-only file.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp") // mode 0600
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
