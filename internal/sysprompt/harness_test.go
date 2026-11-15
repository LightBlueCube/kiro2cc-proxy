package sysprompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var defaults = NewDetector(DefaultSignatures)

func TestDetectReformatted(t *testing.T) {
	for _, tc := range []struct{ system, want string }{
		{"**YOU ARE OPENCODE**,\n\t  an interactive CLI tool", "OpenCode"},
		{"You are Claude Code, Anthropic\u2019s official CLI for Claude.", "Claude Code"},
		{"You are a highly sophisticated automated coding agent with expert\u2011level knowledge across many\ndifferent programming languages and frameworks.", "GitHub Copilot"},
		{"My rules.\n\nYou are `Gemini CLI`, an autonomous CLI agent.", "Gemini CLI"},
	} {
		if got, found := defaults.Detect(tc.system); !found || got != tc.want {
			t.Errorf("Detect(%q) = %q, %v; want %q", tc.system, got, found, tc.want)
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
		if name, found := defaults.Detect(system); found {
			t.Errorf("Detect(%q) = %q, want none", system, name)
		}
	}
}

func TestEditedSignatures(t *testing.T) {
	edited := []Signature{
		{Name: "Mine", Phrases: []string{"You are **My   Agent**"}},
		{Name: "Later", Phrases: []string{"my agent"}},
	}
	if err := ValidateSignatures(edited); err != nil {
		t.Fatal(err)
	}
	d := NewDetector(edited)
	if name, found := d.Detect("you are my agent."); !found || name != "Mine" {
		t.Fatalf("Detect = %q, %v; want the first matching signature, Mine", name, found)
	}
	if edited[0].Phrases[0] != "You are **My   Agent**" {
		t.Fatalf("NewDetector changed its input: %q", edited[0].Phrases[0])
	}
	if name, found := d.Detect("You are opencode, an interactive CLI tool"); found {
		t.Fatalf("Detect = %q, want none: the edited list replaces the defaults", name)
	}

	if err := ValidateSignatures(DefaultSignatures); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for _, bad := range [][]Signature{
		{{Name: " ", Phrases: []string{"x"}}},
		{{Name: "A", Phrases: []string{"x"}}, {Name: "A", Phrases: []string{"y"}}},
		{{Name: "A"}},
		{{Name: "A", Phrases: []string{"x", "** `_` **"}}},
	} {
		if err := ValidateSignatures(bad); err == nil {
			t.Errorf("ValidateSignatures(%+v) passed", bad)
		}
	}
}

// TestCorpus checks the default signatures against prompts collected
// outside the repository (AGYRELAY_HARNESS_CORPUS): <dir>/<harness>/* are
// the harness's default prompts, each detected as that harness;
// <dir>/negative/* (side requests, the scaffolding around a user's own
// prompt, documents naming the harnesses) are detected as none. Every
// phrase must occur in its own harness's prompts and nowhere else, so the
// order of the signatures does not matter.
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

		name, found := defaults.Detect(string(raw))
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

	for _, s := range defaults.signatures {
		for _, phrase := range s.Phrases {
			own := 0
			for group, byPath := range texts {
				for path, text := range byPath {
					if !strings.Contains(text, phrase) {
						continue
					}
					if group != s.Name {
						t.Errorf("%s phrase %q occurs in %s", s.Name, phrase, path)
						continue
					}
					own++
				}
			}
			if own == 0 {
				t.Errorf("%s phrase %q occurs in none of its prompts", s.Name, phrase)
			}
		}
	}
}
