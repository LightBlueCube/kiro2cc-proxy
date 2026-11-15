package sysprompt

import "testing"

func TestFingerprintIgnoresFormatting(t *testing.T) {
	base := Fingerprint("You are **Opencode**,\n\n  a coding   agent.")
	for _, same := range []string{
		"you are opencode, a coding agent.",
		"  You are `opencode`,\ta CODING agent.\n",
	} {
		if got := Fingerprint(same); got != base {
			t.Errorf("Fingerprint(%q) = %s, want %s", same, got, base)
		}
	}
	if Fingerprint("You are opencode, a coding assistant.") == base {
		t.Error("a different wording shares the fingerprint")
	}
	if len(base) != 64 {
		t.Errorf("fingerprint %q is not a hex SHA-256", base)
	}
}
