package agy

import "strings"

// agy injects background-task notifications into the model context as
//
//	The following is a <SYSTEM_MESSAGE> not actually sent by the user...
//	<SYSTEM_MESSAGE> ... </SYSTEM_MESSAGE>
//
// and models (notably Gemini Flash) sometimes echo that block verbatim as
// reply text. sanitizer strips such blocks across streamed deltas.
const (
	systemMessageStart = "The following is a <SYSTEM_MESSAGE>"
	systemMessageEnd   = "</SYSTEM_MESSAGE>"
)

type sanitizer struct {
	buf         string
	suppressing bool
	suppressed  string
	emittedAny  bool
}

// push returns the part of chunk that is safe to emit now.
func (s *sanitizer) push(chunk string) string {
	s.buf += chunk
	var out strings.Builder
	for s.buf != "" {
		if s.suppressing {
			if !s.consumeSuppressed() {
				break
			}
			continue
		}
		start := strings.Index(s.buf, systemMessageStart)
		if start < 0 {
			keep := overlapSuffix(s.buf, systemMessageStart)
			s.emit(&out, s.buf[:len(s.buf)-keep])
			s.buf = s.buf[len(s.buf)-keep:]
			break
		}
		before := s.buf[:start]
		if s.emittedAny || strings.TrimSpace(before) != "" {
			s.emit(&out, strings.TrimSuffix(strings.TrimSuffix(before, "\n"), "\r"))
		}
		s.buf = s.buf[start+len(systemMessageStart):]
		s.suppressing = true
		s.suppressed = systemMessageStart
	}
	return out.String()
}

// consumeSuppressed drops the block once its end marker arrived. false means
// more input is needed.
func (s *sanitizer) consumeSuppressed() bool {
	end := strings.Index(s.buf, systemMessageEnd)
	if end < 0 {
		keep := overlapSuffix(s.buf, systemMessageEnd)
		s.suppressed += s.buf[:len(s.buf)-keep]
		s.buf = s.buf[len(s.buf)-keep:]
		return false
	}
	rest := s.buf[end+len(systemMessageEnd):]
	if s.emittedAny {
		// Keep paragraph spacing: drop one blank line after the block.
		for _, sep := range []string{"\r\n\r\n", "\n\n", "\r\n", "\n"} {
			if strings.HasPrefix(rest, sep) {
				rest = rest[len(sep):]
				break
			}
		}
	} else {
		rest = strings.TrimLeft(rest, "\r\n")
	}
	s.buf = rest
	s.suppressing = false
	s.suppressed = ""
	return true
}

func (s *sanitizer) emit(out *strings.Builder, text string) {
	if text == "" {
		return
	}
	out.WriteString(text)
	s.emittedAny = true
}

// flush returns what is still buffered when the stream ends. An unclosed
// block was not agy's notification after all and is released.
func (s *sanitizer) flush() string {
	rest := s.buf
	if s.suppressing {
		rest = s.suppressed + s.buf
	}
	s.buf, s.suppressed, s.suppressing = "", "", false
	if rest != "" {
		s.emittedAny = true
	}
	return rest
}

// overlapSuffix is the length of the longest suffix of text that is a proper
// prefix of marker.
func overlapSuffix(text, marker string) int {
	limit := min(len(text), len(marker)-1)
	for n := limit; n > 0; n-- {
		if strings.HasPrefix(marker, text[len(text)-n:]) {
			return n
		}
	}
	return 0
}

// agy emits this line as its own message chunk when the model produced a
// malformed tool call, then retries. It is status, not reply text.
const retryNotice = "⚠ Model produced invalid output."

func isRetryNotice(delta string) bool {
	return strings.TrimSpace(delta) == retryNotice
}
