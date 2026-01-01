package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"
)

// PrettyHandler is a lightweight slog handler for console output.
// It produces compact lines and makes level/component easier to read.
//
// Format:
//   15:04:05.000 INF [component] message key=value ...
type PrettyHandler struct {
	w       io.Writer
	mu      *sync.Mutex
	level   slog.Level
	timeFmt string
}

func NewPrettyHandler(w io.Writer, level slog.Level) *PrettyHandler {
	return &PrettyHandler{
		w:       w,
		mu:      &sync.Mutex{},
		level:   level,
		timeFmt: "15:04:05.000",
	}
}

func (h *PrettyHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.level
}

func (h *PrettyHandler) Handle(_ context.Context, r slog.Record) error {
	comp := ""
	attrs := make([]slog.Attr, 0, r.NumAttrs())

	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "comp" || a.Key == "component" {
			comp = fmt.Sprint(a.Value.Any())
			return true
		}
		attrs = append(attrs, a)
		return true
	})

	var b strings.Builder
	ts := r.Time.Local().Format(h.timeFmt)
	b.WriteString(ts)
	b.WriteString(" ")
	b.WriteString(levelShort(r.Level))
	if comp != "" {
		b.WriteString(" [")
		b.WriteString(comp)
		b.WriteString("]")
	}
	b.WriteString(" ")
	b.WriteString(r.Message)

	for _, a := range attrs {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(valString(a.Value))
	}
	b.WriteString("\n")

	h.mu.Lock()
	_, _ = io.WriteString(h.w, b.String())
	h.mu.Unlock()
	return nil
}

func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	return &prettyWithAttrs{base: &cp, attrs: attrs}
}

func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	cp := *h
	return &prettyWithGroup{base: &cp, group: name}
}

type prettyWithAttrs struct {
	base  *PrettyHandler
	attrs []slog.Attr
}

func (p *prettyWithAttrs) Enabled(ctx context.Context, lvl slog.Level) bool { return p.base.Enabled(ctx, lvl) }
func (p *prettyWithAttrs) Handle(ctx context.Context, r slog.Record) error {
	r2 := r.Clone()
	for _, a := range p.attrs {
		r2.AddAttrs(a)
	}
	return p.base.Handle(ctx, r2)
}
func (p *prettyWithAttrs) WithAttrs(attrs []slog.Attr) slog.Handler {
	all := make([]slog.Attr, 0, len(p.attrs)+len(attrs))
	all = append(all, p.attrs...)
	all = append(all, attrs...)
	return &prettyWithAttrs{base: p.base, attrs: all}
}
func (p *prettyWithAttrs) WithGroup(name string) slog.Handler { return (&prettyWithGroup{base: p.base, group: name}) }

type prettyWithGroup struct {
	base  *PrettyHandler
	group string
}

func (p *prettyWithGroup) Enabled(ctx context.Context, lvl slog.Level) bool { return p.base.Enabled(ctx, lvl) }
func (p *prettyWithGroup) Handle(ctx context.Context, r slog.Record) error {
	if p.group == "" {
		return p.base.Handle(ctx, r)
	}

	attrs := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, slog.Attr{Key: p.group + "." + a.Key, Value: a.Value})
		return true
	})

	r3 := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	for _, a := range attrs {
		r3.AddAttrs(a)
	}
	return p.base.Handle(ctx, r3)
}
func (p *prettyWithGroup) WithAttrs(attrs []slog.Attr) slog.Handler {
	return (&prettyWithAttrs{base: p.base, attrs: attrs}).WithGroup(p.group)
}
func (p *prettyWithGroup) WithGroup(name string) slog.Handler {
	if p.group == "" {
		return &prettyWithGroup{base: p.base, group: name}
	}
	return &prettyWithGroup{base: p.base, group: p.group + "." + name}
}

func levelShort(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "DBG"
	case l < slog.LevelWarn:
		return "INF"
	case l < slog.LevelError:
		return "WRN"
	default:
		return "ERR"
	}
}

func valString(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return fmt.Sprintf("%q", v.String())
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	default:
		if v.Kind() == slog.KindAny {
			if err, ok := v.Any().(error); ok {
				return fmt.Sprintf("%q", err.Error())
			}
		}
		return fmt.Sprint(v.Any())
	}
}

// stack returns a best-effort stack trace for panic logs.
func stack() string {
	buf := make([]byte, 64<<10)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}
