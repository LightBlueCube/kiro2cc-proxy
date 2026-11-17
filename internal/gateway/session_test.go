package gateway

import (
	"strings"
	"testing"

	"agyrelay/internal/conv"
)

func TestAppendedPromptSeparatesPolicyAndClientSystem(t *testing.T) {
	tools := []conv.ToolSpec{{Name: "read"}}
	got := appendedPrompt(tools, "Be brief.")

	wantPrefix := additionalPromptOpen + "\nThe client's tools in this session"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("appended prompt does not open with the additional tag and tool policy: %q", got)
	}

	wantBoundary := "\n" + additionalPromptClose + "\n" + clientPromptOpen + "\nBe brief.\n" + clientPromptClose
	if !strings.HasSuffix(got, wantBoundary) {
		t.Errorf("appended prompt does not close the policy block before the client system block: %q", got)
	}

	hasOneAdditionalPair := strings.Count(got, additionalPromptOpen) == 1 && strings.Count(got, additionalPromptClose) == 1
	hasOneClientPair := strings.Count(got, clientPromptOpen) == 1 && strings.Count(got, clientPromptClose) == 1
	if !hasOneAdditionalPair || !hasOneClientPair {
		t.Errorf("appended prompt should hold one additional tag pair and one client tag pair: %q", got)
	}
}

func TestAppendedPromptWithoutClientSystem(t *testing.T) {
	got := appendedPrompt(nil, "")
	want := additionalPromptOpen + "\nThe client offers no tools in this session: answer in text.\n" + additionalPromptClose
	if got != want {
		t.Errorf("appendedPrompt(nil, \"\") = %q; want %q", got, want)
	}
}
