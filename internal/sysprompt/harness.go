// Package sysprompt recognizes the default system prompts of harnesses,
// the coding agents that drive a model (OpenCode, Claude Code, Codex, ...),
// so a request still carrying one can be refused until its client sends a
// system prompt of its own.
//
// A signature names a harness and lists phrases from its prompts: its
// identity line ("You are opencode, ..."), or just its name, which is all
// a crude keyword filter upstream needs. A prompt containing any phrase is
// that harness's. Like upstream's, matching is by substring, except for a
// phrase of at most shortPhrase characters, which matches whole words only:
// "pi" is not found in "api".
package sysprompt

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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

// Match is one occurrence of a signature phrase in a system prompt.
type Match struct {
	Harness string
	// Phrase is the phrase as the signature writes it.
	Phrase string
	// Start and End delimit the occurrence in the prompt as sent, in bytes.
	Start, End int
}

// Detector matches system prompts against signatures.
type Detector struct {
	phrases []phrase
}

// shortPhrase is the longest phrase, in characters, that is too likely to sit
// inside another word ("pi" in "api", "omp" in "compile") to be searched as
// a substring.
const shortPhrase = 3

type phrase struct {
	harness    string
	written    string
	normalized string
	wholeWord  bool
}

func NewDetector(signatures []Signature) *Detector {
	d := &Detector{}
	for _, s := range signatures {
		for _, written := range s.Phrases {
			normalized := normalize(written)
			d.phrases = append(d.phrases, phrase{
				harness: s.Name, written: written, normalized: normalized,
				wholeWord: utf8.RuneCountInString(normalized) <= shortPhrase,
			})
		}
	}
	return d
}

// Detect lists where system contains a phrase, by position. Where phrases
// overlap, the one that starts first, then the longer, is kept.
func (d *Detector) Detect(system string) []Match {
	text := fold(system)
	var found []Match
	for _, p := range d.phrases {
		for from := 0; from < len(text.text); {
			i := strings.Index(text.text[from:], p.normalized)
			if i < 0 {
				break
			}
			start := from + i
			end := start + len(p.normalized)
			if p.wholeWord && !standsAlone(text.text, start, end) {
				from = start + 1
				continue
			}
			found = append(found, Match{
				Harness: p.harness, Phrase: p.written,
				Start: text.from[start], End: text.to[end-1],
			})
			from = end
		}
	}

	slices.SortStableFunc(found, func(a, b Match) int {
		return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(b.End, a.End))
	})
	kept := found[:0]
	for _, m := range found {
		if len(kept) > 0 && m.Start < kept[len(kept)-1].End {
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// standsAlone reports whether text[start:end] is not part of a longer
// word: a word character at an edge of the phrase must not touch another.
func standsAlone(text string, start, end int) bool {
	first, _ := utf8.DecodeRuneInString(text[start:end])
	last, _ := utf8.DecodeLastRuneInString(text[start:end])
	before, _ := utf8.DecodeLastRuneInString(text[:start])
	after, _ := utf8.DecodeRuneInString(text[end:])
	return !(isWordRune(first) && isWordRune(before)) && !(isWordRune(last) && isWordRune(after))
}

// isWordRune is true for Latin letters and ASCII digits. Han and other
// scripts do not join words, so "用opencode写" still contains opencode.
func isWordRune(r rune) bool {
	return '0' <= r && r <= '9' || unicode.IsLetter(r) && unicode.Is(unicode.Latin, r)
}

// foldedRunes maps typographic quotes and dashes to ASCII and drops Markdown
// emphasis and code marks, so a reformatted copy of a prompt still matches.
var foldedRunes = map[rune]string{
	'\u2018': "'", '\u2019': "'", '\u201a': "'", '\u201b': "'", '\u2032': "'",
	'\u201c': `"`, '\u201d': `"`, '\u201e': `"`, '\u2033': `"`,
	'\u2010': "-", '\u2011': "-", '\u2012': "-", '\u2013': "-", '\u2014': "-", '\u2015': "-",
	'\u2026': "...",
	'*':      "", '_': "", '`': "",
}

// folded is normalized text that remembers where each byte came from.
type folded struct {
	text string
	// from[i] and to[i] delimit, in the original, the character that
	// produced text[i].
	from, to []int
}

// fold lowercases text, applies foldedRunes, and collapses every run of
// whitespace (line breaks, indentation) into one space, dropping it at
// either end.
func fold(original string) folded {
	var out strings.Builder
	var f folded
	emit := func(r rune, from, to int) {
		before := out.Len()
		out.WriteRune(r)
		for ; before < out.Len(); before++ {
			f.from = append(f.from, from)
			f.to = append(f.to, to)
		}
	}

	spaceFrom, spaceTo := -1, -1
	for i := 0; i < len(original); {
		r, size := utf8.DecodeRuneInString(original[i:])
		replacement, folds := foldedRunes[r]
		if !folds {
			replacement = string(r)
		}
		for _, c := range replacement {
			if unicode.IsSpace(c) {
				if out.Len() > 0 && spaceFrom < 0 {
					spaceFrom, spaceTo = i, i+size
				}
				continue
			}
			if spaceFrom >= 0 {
				emit(' ', spaceFrom, spaceTo)
				spaceFrom = -1
			}
			emit(unicode.ToLower(c), i, i+size)
		}
		i += size
	}
	f.text = out.String()
	return f
}

func normalize(text string) string {
	return fold(text).text
}
