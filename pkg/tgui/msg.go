package tgui

import (
	"context"
	"strings"
	"unicode/utf8"

	kit "pewbot/internal/transport"

	tele "gopkg.in/telebot.v4"
)

// Message is a rendered UI payload: text + send options.
// It is intended as a single ergonomic "unit" that plugins can build once and
// send/edit without repeating ParseMode/preview/markup boilerplate.
type Message struct {
	Text string
	Opt  *kit.SendOptions

	// More are additional messages to send after the first one.
	// Useful for cases where you want HTML wrappers (e.g. Pre blocks) but also
	// want to guarantee each Telegram message is valid HTML.
	More []string
}

// Send sends the Message via the provided adapter.
// ReplyMarkup is only attached to the first message.
func (m Message) Send(ctx context.Context, ad kit.Adapter, to kit.ChatTarget) (kit.MessageRef, error) {
	if m.Opt == nil {
		m.Opt = &kit.SendOptions{}
	}
	ref, err := ad.SendText(ctx, to, m.Text, m.Opt)
	if err != nil {
		return ref, err
	}

	// Follow-up messages: never attach markup.
	if len(m.More) > 0 {
		opt2 := *m.Opt
		opt2.ReplyMarkupAdapter = nil
		for _, t := range m.More {
			if strings.TrimSpace(t) == "" {
				continue
			}
			if _, e := ad.SendText(ctx, to, t, &opt2); e != nil {
				return ref, e
			}
		}
	}
	return ref, nil
}

// Edit edits the first message referred by ref. If Message has More parts,
// they are sent as new messages after the edit (Telegram cannot edit multiple
// messages at once).
func (m Message) Edit(ctx context.Context, ad kit.Adapter, ref kit.MessageRef, to kit.ChatTarget) error {
	if m.Opt == nil {
		m.Opt = &kit.SendOptions{}
	}
	if err := ad.EditText(ctx, ref, m.Text, m.Opt); err != nil {
		return err
	}
	if len(m.More) > 0 {
		opt2 := *m.Opt
		opt2.ReplyMarkupAdapter = nil
		for _, t := range m.More {
			if strings.TrimSpace(t) == "" {
				continue
			}
			if _, e := ad.SendText(ctx, to, t, &opt2); e != nil {
				return e
			}
		}
	}
	return nil
}

// Builder is the main ergonomic UI builder.
// Default: ParseMode=HTML, DisablePreview=true.
type Builder struct {
	parseMode      string
	disablePreview bool
	rm             *tele.ReplyMarkup
	lines          []string
	more           []string
}

// New creates a new builder with sensible defaults for Telegram.
func New() *Builder {
	return &Builder{parseMode: "HTML", disablePreview: true}
}

// ParseMode overrides Telegram parse mode ("HTML", "Markdown", or empty).
func (b *Builder) ParseMode(mode string) *Builder {
	b.parseMode = strings.TrimSpace(mode)
	return b
}

// DisablePreview sets DisableWebPagePreview.
func (b *Builder) DisablePreview(v bool) *Builder {
	b.disablePreview = v
	return b
}

// Inline attaches an inline keyboard.
func (b *Builder) Inline(kb *Inline) *Builder {
	if kb == nil {
		b.rm = nil
		return b
	}
	b.rm = kb.Markup()
	return b
}

// Title adds a bold title line. Emoji is optional.
func (b *Builder) Title(emoji, title string) *Builder {
	e := strings.TrimSpace(emoji)
	t := strings.TrimSpace(title)
	if t == "" {
		return b
	}
	if strings.EqualFold(b.parseMode, "HTML") {
		if e != "" {
			b.lines = append(b.lines, Esc(e).String()+" "+wrap("b", Esc(t)).String())
		} else {
			b.lines = append(b.lines, wrap("b", Esc(t)).String())
		}
		return b
	}
	// plain / markdown
	if e != "" {
		b.lines = append(b.lines, e+" "+t)
	} else {
		b.lines = append(b.lines, t)
	}
	return b
}

// Section adds a section header.
func (b *Builder) Section(title string) *Builder {
	t := strings.TrimSpace(title)
	if t == "" {
		return b
	}
	if strings.EqualFold(b.parseMode, "HTML") {
		b.lines = append(b.lines, wrap("b", Esc(t)).String())
		return b
	}
	b.lines = append(b.lines, t)
	return b
}

// Line adds a single line, escaping when ParseMode is HTML.
func (b *Builder) Line(s string) *Builder {
	if strings.TrimSpace(s) == "" {
		b.lines = append(b.lines, "")
		return b
	}
	if strings.EqualFold(b.parseMode, "HTML") {
		b.lines = append(b.lines, Esc(s).String())
	} else {
		b.lines = append(b.lines, s)
	}
	return b
}

// RawLine appends a line without escaping. Only use if you know what you're doing.
func (b *Builder) RawLine(s string) *Builder {
	b.lines = append(b.lines, s)
	return b
}

// Blank inserts an empty line.
func (b *Builder) Blank() *Builder { return b.Line("") }

// Bullets adds bullet lines.
func (b *Builder) Bullets(items ...string) *Builder {
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		b.Line("• " + it)
	}
	return b
}

// KV adds a "key: value" row with consistent formatting.
func (b *Builder) KV(key, value string) *Builder {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" {
		return b
	}
	if strings.EqualFold(b.parseMode, "HTML") {
		// bold key, value escaped.
		b.lines = append(b.lines, "• "+wrap("b", Esc(key)).String()+": "+Esc(value).String())
		return b
	}
	if value == "" {
		b.lines = append(b.lines, "• "+key)
	} else {
		b.lines = append(b.lines, "• "+key+": "+value)
	}
	return b
}

// Code adds an inline <code>...</code> line when ParseMode is HTML.
// For non-HTML parse modes it falls back to plain text.
func (b *Builder) Code(s string) *Builder {
	s = strings.TrimSpace(s)
	if s == "" {
		return b
	}
	if strings.EqualFold(b.parseMode, "HTML") {
		// Code() already escapes. Use RawLine to avoid double escaping.
		b.lines = append(b.lines, Code(s).String())
		return b
	}
	b.lines = append(b.lines, s)
	return b
}

// Pre adds a preformatted block. For very long content, use PreMulti.
func (b *Builder) Pre(code string) *Builder {
	code = strings.TrimRight(code, "\n")
	if code == "" {
		return b
	}
	if strings.EqualFold(b.parseMode, "HTML") {
		b.lines = append(b.lines, Pre(code).String())
		return b
	}
	// Plain text fallback.
	b.lines = append(b.lines, code)
	return b
}

// PreMulti renders a long pre block into multiple Telegram-safe messages.
// Each chunk is individually wrapped with <pre><code>...</code></pre> (HTML).
func (b *Builder) PreMulti(code string, chunkLimit ...int) *Builder {
	code = strings.TrimRight(code, "\n")
	if code == "" {
		return b
	}
	limit := 3500
	if len(chunkLimit) > 0 && chunkLimit[0] > 0 {
		limit = chunkLimit[0]
	}
	// Only implemented for HTML; for other parse modes, just append as-is.
	if !strings.EqualFold(b.parseMode, "HTML") {
		b.lines = append(b.lines, code)
		return b
	}

	// Telegram has a hard limit at 4096 characters for a single message.
	// We chunk by runes to keep it Unicode-safe without allocating a full []rune
	// for large content.
	const preWrapperOverhead = 24 // len("<pre><code></code></pre>")
	eff := limit - preWrapperOverhead
	if eff < 128 {
		eff = 128
	}

	start := 0 // byte index
	first := true
	for start < len(code) {
		runes := 0
		end := start
		lastNL := -1 // byte index after last newline rune in this window
		lastNLRunes := 0
		for end < len(code) && runes < eff {
			r, size := utf8.DecodeRuneInString(code[end:])
			// utf8.DecodeRuneInString never returns size 0.
			if r == '\n' {
				lastNL = end + size
				lastNLRunes = runes + 1
			}
			runes++
			end += size
		}
		if end < len(code) && lastNL != -1 {
			// Prefer a newline boundary close to the end of the window.
			if lastNLRunes >= eff/3 {
				end = lastNL
			}
		}
		chunk := strings.TrimRight(code[start:end], "\n")
		if first {
			b.lines = append(b.lines, Pre(chunk).String())
			first = false
		} else {
			b.more = append(b.more, Pre(chunk).String())
		}
		start = end
		// Skip extra newlines at the start of the next chunk.
		for start < len(code) {
			r, size := utf8.DecodeRuneInString(code[start:])
			if r != '\n' {
				break
			}
			start += size
		}
	}
	return b
}

// Build produces a ready-to-send Message.
func (b *Builder) Build() Message {
	// Extract follow-up PRE chunks.
	mainLines := make([]string, 0, len(b.lines))
	more := make([]string, 0, len(b.more)+4)
	more = append(more, b.more...)
	for _, ln := range b.lines {
		if strings.HasPrefix(ln, "\x1ePRE_MORE\x1e") {
			more = append(more, strings.TrimPrefix(ln, "\x1ePRE_MORE\x1e"))
			continue
		}
		mainLines = append(mainLines, ln)
	}
	text := strings.Join(mainLines, "\n")
	text = strings.Trim(text, "\n")

	opt := &kit.SendOptions{ParseMode: b.parseMode, DisablePreview: b.disablePreview}
	if b.rm != nil {
		opt.ReplyMarkupAdapter = b.rm
	}
	return Message{Text: text, Opt: opt, More: more}
}
