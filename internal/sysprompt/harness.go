// Package sysprompt recognizes the default system prompts of harnesses,
// the coding agents that drive a model (OpenCode, Claude Code, Codex, ...),
// so a request still carrying one can be refused until its client sends a
// system prompt of its own.
//
// Each harness has a few phrases from its own prompts, chiefly its identity
// line ("You are opencode, ..."), which survives the prompt's revisions.
// Phrases come from the replaceable main prompt only, not from what the
// harness adds around a prompt the user supplies (environment, skills) nor
// from its side requests (titles, summaries): those stay when the user
// replaces the prompt.
package sysprompt

import "strings"

type signature struct {
	name    string
	phrases []string
}

// DetectHarness names the harness whose default prompt system contains;
// found is false when none does.
func DetectHarness(system string) (name string, found bool) {
	text := normalize(system)
	for _, s := range signatures {
		for _, phrase := range s.phrases {
			if strings.Contains(text, phrase) {
				return s.name, true
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

func compile(raw []signature) []signature {
	for i := range raw {
		for j, phrase := range raw[i].phrases {
			raw[i].phrases[j] = normalize(phrase)
		}
	}
	return raw
}
