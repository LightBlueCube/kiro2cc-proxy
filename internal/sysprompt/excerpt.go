package sysprompt

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// excerptContext is how many characters around a match an excerpt
	// keeps when its paragraph is longer.
	excerptContext = 120
	maxExcerpts    = 5

	markOpen  = ">>"
	markClose = "<<"
	ellipsis  = "..."
)

var paragraphBreak = regexp.MustCompile(`\n[ \t\r]*\n`)

// Harnesses names the harnesses of matches, each once, in order of
// appearance.
func Harnesses(matches []Match) []string {
	var names []string
	for _, m := range matches {
		if !slices.Contains(names, m.Harness) {
			names = append(names, m.Harness)
		}
	}
	return names
}

// excerpt is the stretch of one paragraph around some matches.
type excerpt struct {
	from, to      int // the stretch kept
	paragraphFrom int
	paragraphTo   int
	matches       []Match
}

// Excerpts quotes where matches sit in system, the paragraph around each
// with the matches marked, so the client sees what to change. Matches
// close together share one excerpt; past maxExcerpts the rest is only
// counted. matches are Detect's result for system.
func Excerpts(system string, matches []Match) string {
	if len(matches) == 0 {
		return ""
	}
	breaks := paragraphBreak.FindAllStringIndex(system, -1)

	var excerpts []*excerpt
	for _, m := range matches {
		paragraphFrom, paragraphTo := paragraphAround(system, breaks, m)
		from := stepBack(system, m.Start, excerptContext, paragraphFrom)
		to := stepForward(system, m.End, excerptContext, paragraphTo)

		if n := len(excerpts); n > 0 && from <= excerpts[n-1].to {
			last := excerpts[n-1]
			last.to = max(last.to, to)
			last.matches = append(last.matches, m)
			continue
		}
		excerpts = append(excerpts, &excerpt{
			from: from, to: to,
			paragraphFrom: paragraphFrom, paragraphTo: paragraphTo,
			matches: []Match{m},
		})
	}

	var quoted []string
	omitted := 0
	for i, e := range excerpts {
		if i >= maxExcerpts {
			omitted += len(e.matches)
			continue
		}
		quoted = append(quoted, e.quote(system))
	}

	text := "matched text (marked with " + markOpen + " " + markClose + "):\n\n" + strings.Join(quoted, "\n\n")
	if omitted > 0 {
		text += fmt.Sprintf("\n\nand %d more", omitted)
	}
	return text
}

func (e *excerpt) quote(system string) string {
	var out strings.Builder
	if e.from > e.paragraphFrom {
		out.WriteString(ellipsis)
	}
	cursor := e.from
	for _, m := range e.matches {
		out.WriteString(system[cursor:m.Start])
		out.WriteString(markOpen + system[m.Start:m.End] + markClose)
		cursor = m.End
	}
	out.WriteString(system[cursor:e.to])
	if e.to < e.paragraphTo {
		out.WriteString(ellipsis)
	}
	return out.String()
}

// paragraphAround returns the bounds of the paragraph holding m, without
// the whitespace around it. A paragraph ends at a blank line, unless the
// match itself spans it.
func paragraphAround(system string, breaks [][]int, m Match) (from, to int) {
	from, to = 0, len(system)
	for _, b := range breaks {
		if b[1] <= m.Start {
			from = b[1]
			continue
		}
		if b[0] >= m.End {
			to = b[0]
			break
		}
	}
	paragraph := system[from:to]
	leading := len(paragraph) - len(strings.TrimLeftFunc(paragraph, unicode.IsSpace))
	return from + leading, from + len(strings.TrimRightFunc(paragraph, unicode.IsSpace))
}

func stepBack(text string, pos, characters, floor int) int {
	for ; characters > 0 && pos > floor; characters-- {
		_, size := utf8.DecodeLastRuneInString(text[:pos])
		pos -= size
	}
	return pos
}

func stepForward(text string, pos, characters, ceiling int) int {
	for ; characters > 0 && pos < ceiling; characters-- {
		_, size := utf8.DecodeRuneInString(text[pos:])
		pos += size
	}
	return pos
}
