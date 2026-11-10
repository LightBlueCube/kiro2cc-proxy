// Package logbuf is a slog handler that also keeps the newest log entries
// in memory, for the admin UI to list, filter, and follow.
package logbuf

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// Attr is a flattened attribute; group names prefix the key ("a.b").
type Attr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Entry struct {
	Seq   int64      `json:"seq"`
	Time  time.Time  `json:"time"`
	Level slog.Level `json:"-"`
	// LevelName is Level for the UI ("INFO").
	LevelName string `json:"level"`
	Message   string `json:"message"`
	Attrs     []Attr `json:"attrs"`
}

// Attr returns the value of the attribute key, or "".
func (e Entry) Attr(key string) string {
	for _, a := range e.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

// Filter selects entries; zero fields match all.
type Filter struct {
	MinLevel  slog.Level
	Component string
	Account   string
	// Text matches the message or any attribute value, ignoring case.
	Text string
}

func (f Filter) Match(e Entry) bool {
	if e.Level < f.MinLevel {
		return false
	}
	if f.Component != "" && e.Attr("component") != f.Component {
		return false
	}
	if f.Account != "" && e.Attr("account") != f.Account {
		return false
	}
	if f.Text == "" {
		return true
	}
	text := strings.ToLower(f.Text)
	if strings.Contains(strings.ToLower(e.Message), text) {
		return true
	}
	return slices.ContainsFunc(e.Attrs, func(a Attr) bool {
		return strings.Contains(strings.ToLower(a.Key+"="+a.Value), text)
	})
}

// Buffer is a ring of the newest entries plus live subscribers.
type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	size    int
	start   int // index of the oldest entry once full
	seq     int64
	subs    map[chan Entry]struct{}
}

func NewBuffer(size int) *Buffer {
	return &Buffer{size: size, subs: map[chan Entry]struct{}{}}
}

func (b *Buffer) add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if len(b.entries) < b.size {
		b.entries = append(b.entries, e)
	} else {
		b.entries[b.start] = e
		b.start = (b.start + 1) % b.size
	}
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // a slow reader misses entries rather than blocking logging
		}
	}
}

// Since returns matching entries after seq, oldest first, at most limit
// (the newest ones).
func (b *Buffer) Since(seq int64, f Filter, limit int) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Entry
	for i := range b.entries {
		e := b.entries[(b.start+i)%len(b.entries)]
		if e.Seq > seq && f.Match(e) {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe delivers new entries until cancel is called.
func (b *Buffer) Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Handler writes records to next and adds them to the buffer.
type Handler struct {
	buf    *Buffer
	next   slog.Handler
	level  slog.Leveler
	attrs  []Attr
	prefix string // group path for attributes added later
}

func NewHandler(buf *Buffer, next slog.Handler, level slog.Leveler) *Handler {
	return &Handler{buf: buf, next: next, level: level}
}

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.next.Handle(ctx, r)
	e := Entry{Time: r.Time, Level: r.Level, LevelName: r.Level.String(), Message: r.Message, Attrs: slices.Clone(h.attrs)}
	r.Attrs(func(a slog.Attr) bool {
		e.Attrs = flatten(e.Attrs, h.prefix, a)
		return true
	})
	h.buf.add(e)
	return err
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.next = h.next.WithAttrs(attrs)
	clone.attrs = slices.Clone(h.attrs)
	for _, a := range attrs {
		clone.attrs = flatten(clone.attrs, h.prefix, a)
	}
	return &clone
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.next = h.next.WithGroup(name)
	clone.prefix = h.prefix + name + "."
	return &clone
}

func flatten(out []Attr, prefix string, a slog.Attr) []Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		inner := prefix
		if a.Key != "" {
			inner += a.Key + "."
		}
		for _, g := range v.Group() {
			out = flatten(out, inner, g)
		}
		return out
	}
	if a.Key == "" {
		return out
	}
	value := v.String()
	if v.Kind() == slog.KindAny {
		if err, ok := v.Any().(error); ok {
			value = err.Error()
		} else {
			value = fmt.Sprint(v.Any())
		}
	}
	return append(out, Attr{Key: prefix + a.Key, Value: value})
}
