package sysprompt

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
		matches := defaults.Detect(tc.system)
		if len(matches) == 0 || matches[0].Harness != tc.want {
			t.Errorf("Detect(%q) = %+v; want %q first", tc.system, matches, tc.want)
		}
	}
}

// TestDetectWords checks that a name which is no common word is searched as
// a substring, as upstream does, and the names listed as Words only when they
// touch no other word character or ASCII punctuation.
func TestDetectWords(t *testing.T) {
	for _, system := range []string{
		"",
		"You are a helpful assistant.",
		"The REST API returns JSON; sign in as root in the room, then compile it.",
		"Run the sample, optimized: decline, mongoose, Traefik, 3 kilobytes, &amp; zeds.",
		"Amp, Cursor, Crush, Goose, Augment and Kilo are common words: move the cursor, crush it.",
		"See ~/.config/opencode/agent and /work/opencode/out.",
		"You are xopencode",
	} {
		if matches := defaults.Detect(system); len(matches) > 0 {
			t.Errorf("Detect(%q) = %+v, want none", system, matches)
		}
	}

	for _, tc := range []struct{ system, want string }{
		{"用opencode写代码", "OpenCode"},
		{"- opencode (8 tools) // Tools for managing OpenCode itself", "OpenCode"},
		{"See opencode.json and tools.opencode.list.", "OpenCode"},
		{"You are opencodes", "OpenCode"},
		{"inside pi now", "pi"},
		{"pi-coding-agent", "pi"},
		{"Run omp now", "oh-my-pi"},
		{"Kilo Code and the Claude Code CLI", "Kilo Code"},
		{"Ask Cline or Roo or Zed now", "Cline"},
		{"Use Trae rather than Traefik", "Trae"},
	} {
		matches := defaults.Detect(tc.system)
		if len(matches) == 0 || matches[0].Harness != tc.want {
			t.Errorf("Detect(%q) = %+v; want %q first", tc.system, matches, tc.want)
		}
	}
}

func TestDetectPositions(t *testing.T) {
	d := NewDetector([]Signature{
		{Name: "Opener", Phrases: []string{"You are opencode,", "opencode"}},
		{Name: "Kilo", Phrases: []string{"kilo code"}},
	})
	system := "Be **brief**.\n\n  You are **OpenCode**,\n  a tool. Not  Kilo\n\tCode. OpenCode."
	var got []string
	for _, m := range d.Detect(system) {
		got = append(got, fmt.Sprintf("%s:%s:%q", m.Harness, m.Phrase, system[m.Start:m.End]))
	}
	want := []string{
		`Opener:You are opencode,:"You are **OpenCode**,"`,
		"Kilo:kilo code:\"Kilo\\n\\tCode\"",
		`Opener:opencode:"OpenCode"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("matches:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExcerpts(t *testing.T) {
	d := NewDetector([]Signature{{Name: "Opener", Phrases: []string{"opencode"}}})
	long := strings.Repeat("word ", 100)

	for _, tc := range []struct{ name, system, want string }{
		{
			"the paragraph holding the match",
			"Be brief.\n\nUse OpenCode here, and\ncontinue.\n\nBye.",
			"Use >>OpenCode<< here, and\ncontinue.",
		},
		{
			"two matches in a paragraph share an excerpt",
			"Run opencode, then opencode again.",
			"Run >>opencode<<, then >>opencode<< again.",
		},
	} {
		got := Excerpts(tc.system, d.Detect(tc.system))
		if want := "matched text (marked with >> <<):\n\n" + tc.want; got != want {
			t.Errorf("%s: got %q, want %q", tc.name, got, want)
		}
	}

	cut := Excerpts(long+"see opencode now. "+long, d.Detect(long+"see opencode now. "+long))
	if !strings.HasPrefix(cut, "matched text (marked with >> <<):\n\n...") ||
		!strings.Contains(cut, "see >>opencode<< now.") || !strings.HasSuffix(cut, "...") || len(cut) > 400 {
		t.Errorf("a long paragraph is cut around the match: got %q", cut)
	}

	var paragraphs []string
	for i := range 8 {
		paragraphs = append(paragraphs, fmt.Sprintf("part %d: opencode", i))
	}
	system := strings.Join(paragraphs, "\n\n")
	got := Excerpts(system, d.Detect(system))
	if strings.Count(got, ">>opencode<<") != maxExcerpts || !strings.HasSuffix(got, "\n\nand 3 more") {
		t.Errorf("many paragraphs: got %q", got)
	}
	if got := Excerpts("clean", nil); got != "" {
		t.Errorf("no matches: got %q", got)
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
	if matches := d.Detect("you are my agent."); len(matches) != 1 || matches[0].Harness != "Mine" {
		t.Fatalf("Detect = %+v; want the longer phrase, Mine, alone", matches)
	}
	if edited[0].Phrases[0] != "You are **My   Agent**" {
		t.Fatalf("NewDetector changed its input: %q", edited[0].Phrases[0])
	}
	if matches := d.Detect("You are opencode, an interactive CLI tool"); len(matches) > 0 {
		t.Fatalf("Detect = %+v, want none: the edited list replaces the defaults", matches)
	}

	wordsOnly := []Signature{{Name: "Short", Words: []string{"Ab"}}}
	if err := ValidateSignatures(wordsOnly); err != nil {
		t.Fatalf("a signature of words alone: %v", err)
	}
	words := NewDetector(wordsOnly)
	if matches := words.Detect("cab, abs, xab, /ab/, ab.json, (ab)"); len(matches) > 0 {
		t.Fatalf("Detect = %+v, want none: a word is not found inside another or touching punctuation", matches)
	}
	if matches := words.Detect("cab, `AB` now"); len(matches) != 1 || matches[0].Phrase != "Ab" {
		t.Fatalf("Detect = %+v, want the word Ab once", matches)
	}

	if err := ValidateSignatures(DefaultSignatures); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for _, bad := range [][]Signature{
		{{Name: " ", Phrases: []string{"x"}}},
		{{Name: "A", Phrases: []string{"x"}}, {Name: "A", Phrases: []string{"y"}}},
		{{Name: "A"}},
		{{Name: "A", Phrases: []string{"x", "** `_` **"}}},
		{{Name: "A", Phrases: []string{"x"}, Words: []string{"` _"}}},
	} {
		if err := ValidateSignatures(bad); err == nil {
			t.Errorf("ValidateSignatures(%+v) passed", bad)
		}
	}
}

// TestCorpus checks the default signatures against prompts collected
// outside the repository (AGYRELAY_HARNESS_CORPUS): <dir>/<harness>/* are
// the harness's default prompts, each of which its own signature must
// match. <dir>/negative/* (side requests, scaffolding, documents) is
// reported, not asserted: a text naming a harness matches by design.
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
	harnesses := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		group := filepath.Base(filepath.Dir(path))
		matches := defaults.Detect(string(raw))
		if group == negative {
			t.Logf("%s: %v", filepath.Base(path), Harnesses(matches))
			continue
		}
		harnesses++
		if !slices.Contains(Harnesses(matches), group) {
			t.Errorf("%s: matched %v, want %q among them", path, Harnesses(matches), group)
		}
	}
	if harnesses == 0 {
		t.Fatalf("corpus %s has no harness prompts", dir)
	}
}
