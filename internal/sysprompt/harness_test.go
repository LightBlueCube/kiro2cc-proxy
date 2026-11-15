package sysprompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectReformatted(t *testing.T) {
	for _, tc := range []struct{ system, want string }{
		{"**YOU ARE OPENCODE**,\n\t  an interactive CLI tool", "OpenCode"},
		{"You are Claude Code, Anthropic\u2019s official CLI for Claude.", "Claude Code"},
		{"You are a highly sophisticated automated coding agent with expert\u2011level knowledge across many\ndifferent programming languages and frameworks.", "GitHub Copilot"},
		{"My rules.\n\nYou are `Gemini CLI`, an autonomous CLI agent.", "Gemini CLI"},
	} {
		if got, found := DetectHarness(tc.system); !found || got != tc.want {
			t.Errorf("DetectHarness(%q) = %q, %v; want %q", tc.system, got, found, tc.want)
		}
	}
}

func TestDetectOwnPrompt(t *testing.T) {
	for _, system := range []string{
		"",
		"You are a helpful assistant.",
		"Run the tests with opencode; Claude Code and Codex users read AGENTS.md.",
		"You are Kiro, a general-purpose agentic AI assistant.",
	} {
		if agent, found := DetectHarness(system); found {
			t.Errorf("DetectHarness(%q) = %q, want none", system, agent)
		}
	}
}

// TestCorpus checks the signatures against prompts collected outside the
// repository (AGYRELAY_HARNESS_CORPUS): <dir>/<harness>/* are the harness's
// default prompts, each detected as that harness; <dir>/negative/* (side
// requests, the scaffolding around a user's own prompt, documents naming
// the harnesses) are detected as none. Every phrase must occur in its own
// harness's prompts and nowhere else, so the order of signatures is moot.
func TestCorpus(t *testing.T) {
	dir := os.Getenv("AGYRELAY_HARNESS_CORPUS")
	if dir == "" {
		t.Skip("AGYRELAY_HARNESS_CORPUS is not set")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	const negative = "negative"
	texts := map[string]map[string]string{} // group → path → normalized text
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		group := filepath.Base(filepath.Dir(path))
		if texts[group] == nil {
			texts[group] = map[string]string{}
		}
		texts[group][path] = normalize(string(raw))

		name, found := DetectHarness(string(raw))
		switch {
		case group == negative && found:
			t.Errorf("%s: detected %s, want none", path, name)
		case group != negative && name != group:
			t.Errorf("%s: detected %q, want %q", path, name, group)
		}
	}
	if len(texts) < 2 || texts[negative] == nil {
		t.Fatalf("corpus %s has no harnesses or no %s/ directory", dir, negative)
	}

	for _, s := range signatures {
		for _, phrase := range s.phrases {
			own := 0
			for group, byPath := range texts {
				for path, text := range byPath {
					if !strings.Contains(text, phrase) {
						continue
					}
					if group != s.name {
						t.Errorf("%s phrase %q occurs in %s", s.name, phrase, path)
						continue
					}
					own++
				}
			}
			if own == 0 {
				t.Errorf("%s phrase %q occurs in none of its prompts", s.name, phrase)
			}
		}
	}
}
