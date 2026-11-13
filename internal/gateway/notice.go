package gateway

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// agy reports some failures as ordinary reply text with stopReason
// end_turn. Such a reply must become an error, so the start of every
// response is held while it could still be one of these notices.
const (
	quotaNoticePrefix    = "Usage Limit Reached"
	capacityNoticePrefix = "⚠ Our servers are experiencing high traffic"
	// agentErrorNotice ends a turn whose model call failed; the cause is
	// in the session's ERROR_MESSAGE step.
	agentErrorNotice = "⚠ Agent execution terminated due to error."
)

var noticePrefixes = []string{quotaNoticePrefix, capacityNoticePrefix, agentErrorNotice}

// contextOverflow matches the model errors seen for a conversation over
// the window (agy 1.5.3):
//
//	Gemini:       The input token count exceeds the maximum number of tokens allowed 1048576.
//	gpt-oss-120b: Requested token count exceeds the model's maximum context length of 131072 tokens. …
//	gpt-oss-120b: The input (183641 tokens) is longer than the model's context length (131072 tokens).
var contextOverflow = regexp.MustCompile(`exceeds the maximum number of tokens allowed|exceeds the model's maximum context length|is longer than the model's context length`)

var quotaNotice = regexp.MustCompile(`^Usage Limit Reached\n\nYou have reached your current quota for this period\.(?: Your limit will reset (?:in ((?:\d+ (?:day|hour|minute|second)s?(?:, )?)+)|on (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) (\d{1,2}), (\d{4}) (\d{2}):(\d{2}) UTC)\.)?$`)

var capacityNotice = regexp.MustCompile(`^⚠\s*(Our servers are experiencing high traffic[^\n]*)$`)

var relativeUnit = regexp.MustCompile(`(\d+) (day|hour|minute|second)`)

var unitDuration = map[string]time.Duration{"day": 24 * time.Hour, "hour": time.Hour, "minute": time.Minute, "second": time.Second}

type guardState int

const (
	guardUndecided guardState = iota
	guardText
	guardNotice
)

// noticeGuard holds response text while it may still be a notice.
type noticeGuard struct {
	held  strings.Builder
	state guardState
}

// push returns the text that can be emitted now.
func (g *noticeGuard) push(delta string) string {
	if g.state == guardText {
		return delta
	}
	g.held.WriteString(delta)
	if g.state == guardNotice {
		return ""
	}
	text := strings.TrimLeft(g.held.String(), " \t\r\n")
	for _, prefix := range noticePrefixes {
		if strings.HasPrefix(text, prefix) {
			g.state = guardNotice
			return ""
		}
		if strings.HasPrefix(prefix, text) {
			return "" // still ambiguous
		}
	}
	return g.flush()
}

// flush ends holding: whatever was held is ordinary text.
func (g *noticeGuard) flush() string {
	g.state = guardText
	text := g.held.String()
	g.held.Reset()
	return text
}

type noticeKind int

const (
	noticeNone noticeKind = iota
	noticeQuota
	noticeCapacity
	noticeAgentError
)

// classifyNotice classifies a whole reply. For a quota notice it returns
// the reset time (zero when unstated); for a capacity notice its message.
func classifyNotice(text string, now time.Time) (noticeKind, time.Time, string) {
	trimmed := strings.TrimSpace(text)
	if m := quotaNotice.FindStringSubmatch(trimmed); m != nil {
		return noticeQuota, quotaReset(m, now), trimmed
	}
	if m := capacityNotice.FindStringSubmatch(trimmed); m != nil {
		return noticeCapacity, time.Time{}, m[1]
	}
	if trimmed == agentErrorNotice {
		return noticeAgentError, time.Time{}, trimmed
	}
	return noticeNone, time.Time{}, ""
}

var months = map[string]time.Month{"Jan": 1, "Feb": 2, "Mar": 3, "Apr": 4, "May": 5, "Jun": 6, "Jul": 7, "Aug": 8, "Sep": 9, "Oct": 10, "Nov": 11, "Dec": 12}

// quotaReset is when the quota resets, or the zero time when unstated.
// The regexp guarantees every number parses.
func quotaReset(m []string, now time.Time) time.Time {
	if relative := m[1]; relative != "" {
		var total time.Duration
		for _, part := range relativeUnit.FindAllStringSubmatch(relative, -1) {
			n, _ := strconv.Atoi(part[1])
			total += time.Duration(n) * unitDuration[part[2]]
		}
		return now.Add(total)
	}
	if month := m[2]; month != "" {
		day, _ := strconv.Atoi(m[3])
		year, _ := strconv.Atoi(m[4])
		hour, _ := strconv.Atoi(m[5])
		minute, _ := strconv.Atoi(m[6])
		return time.Date(year, months[month], day, hour, minute, 0, 0, time.UTC)
	}
	return time.Time{}
}
