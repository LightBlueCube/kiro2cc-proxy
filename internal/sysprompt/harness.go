// Package sysprompt recognizes the default system prompts of harnesses,
// the coding agents that drive a model (OpenCode, Claude Code, Codex, ...),
// so a request still carrying one can be refused until its client sends a
// system prompt of its own.
//
// A signature names a harness and lists phrases from its prompts, chiefly
// its identity line ("You are opencode, ..."), which survives the prompt's
// revisions. A prompt containing any phrase is that harness's. Phrases
// should come from the replaceable main prompt only, not from what the
// harness adds around a prompt the user supplies (environment, skills) nor
// from its side requests (titles, summaries): those stay when the user
// replaces the prompt.
package sysprompt

import (
	"errors"
	"fmt"
	"strings"
)

// Signature recognizes one harness's default prompts.
type Signature struct {
	Name    string   `json:"name"`
	Phrases []string `json:"phrases"`
}

// ValidateSignatures checks an edited list: every signature has a unique
// name and phrases that keep some text once normalized.
func ValidateSignatures(signatures []Signature) error {
	seen := map[string]bool{}
	for _, s := range signatures {
		if strings.TrimSpace(s.Name) == "" {
			return errors.New("every harness signature needs a name")
		}
		if seen[s.Name] {
			return fmt.Errorf("%s has two harness signatures", s.Name)
		}
		seen[s.Name] = true
		if len(s.Phrases) == 0 {
			return fmt.Errorf("the harness signature of %s needs a phrase", s.Name)
		}
		for _, phrase := range s.Phrases {
			if normalize(phrase) == "" {
				return fmt.Errorf("the harness signature of %s has an empty phrase", s.Name)
			}
		}
	}
	return nil
}

// Detector matches system prompts against signatures.
type Detector struct {
	// signatures hold normalized phrases.
	signatures []Signature
}

func NewDetector(signatures []Signature) *Detector {
	compiled := make([]Signature, len(signatures))
	for i, s := range signatures {
		phrases := make([]string, len(s.Phrases))
		for j, phrase := range s.Phrases {
			phrases[j] = normalize(phrase)
		}
		compiled[i] = Signature{Name: s.Name, Phrases: phrases}
	}
	return &Detector{signatures: compiled}
}

// Detect names the first harness, in list order, with a phrase that system
// contains; found is false when there is none.
func (d *Detector) Detect(system string) (name string, found bool) {
	text := normalize(system)
	for _, s := range d.signatures {
		for _, phrase := range s.Phrases {
			if strings.Contains(text, phrase) {
				return s.Name, true
			}
		}
	}
	return "", false
}

// folds maps typographic quotes and dashes to ASCII and drops Markdown
// emphasis and code marks, so a reformatted copy of a prompt still matches.
var folds = strings.NewReplacer(
	"\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'", "\u2032", "'",
	"\u201c", `"`, "\u201d", `"`, "\u201e", `"`, "\u2033", `"`,
	"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2015", "-",
	"\u2026", "...",
	"*", "", "_", "", "`", "",
)

// normalize lowercases text, applies folds, and collapses every run of
// whitespace (line breaks, indentation) into one space.
func normalize(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(folds.Replace(text))), " ")
}
