package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Ring is a slog.Handler that keeps the most recent log lines in memory (for
// display in the admin dashboard) while delegating to an inner handler for
// normal output.
type Ring struct {
	inner slog.Handler
	buf   *ringBuffer
}

type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// NewRing wraps inner, retaining up to max recent formatted lines.
func NewRing(inner slog.Handler, max int) *Ring {
	if max < 1 {
		max = 200
	}
	return &Ring{inner: inner, buf: &ringBuffer{max: max}}
}

func (r *Ring) Enabled(ctx context.Context, l slog.Level) bool { return r.inner.Enabled(ctx, l) }

func (r *Ring) Handle(ctx context.Context, rec slog.Record) error {
	r.buf.add(formatRecord(rec))
	return r.inner.Handle(ctx, rec)
}

func (r *Ring) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Ring{inner: r.inner.WithAttrs(attrs), buf: r.buf}
}

func (r *Ring) WithGroup(name string) slog.Handler {
	return &Ring{inner: r.inner.WithGroup(name), buf: r.buf}
}

// Lines returns a copy of the retained log lines, oldest first.
func (r *Ring) Lines() []string { return r.buf.snapshot() }

func (b *ringBuffer) add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

func (b *ringBuffer) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

func formatRecord(rec slog.Record) string {
	var sb strings.Builder
	sb.WriteString(rec.Time.Format(time.RFC3339))
	sb.WriteString(" ")
	sb.WriteString(rec.Level.String())
	sb.WriteString(" ")
	sb.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	return sb.String()
}
