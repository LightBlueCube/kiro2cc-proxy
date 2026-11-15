package sysprompt

import (
	"crypto/sha256"
	"encoding/hex"
)

// Fingerprint identifies a system prompt across reformatting: the hex
// SHA-256 of its text folded as Detect folds it, so case, spacing, line
// breaks, and Markdown emphasis do not change it.
func Fingerprint(system string) string {
	sum := sha256.Sum256([]byte(fold(system).text))
	return hex.EncodeToString(sum[:])
}
