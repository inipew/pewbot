package tgui

import (
	"fmt"
	"html"
	"strings"
)

// H represents HTML that is safe to pass to Telegram when ParseMode="HTML".
// Values of type H should be treated as already-escaped.
type H string

func (h H) String() string { return string(h) }

// Esc escapes text for Telegram HTML parse mode.
func Esc(s string) H { return H(html.EscapeString(s)) }

// Raw marks a string as already-safe HTML.
// Use sparingly.
func Raw(s string) H { return H(s) }

func wrap(tag string, inner H) H { return H("<" + tag + ">" + inner.String() + "</" + tag + ">") }

func B(s string) H     { return wrap("b", Esc(s)) }
func I(s string) H     { return wrap("i", Esc(s)) }
func U(s string) H     { return wrap("u", Esc(s)) }
func S(s string) H     { return wrap("s", Esc(s)) }
func Code(s string) H  { return wrap("code", Esc(s)) }
func Quote(s string) H { return wrap("blockquote", Esc(s)) }

// Pre renders a preformatted block.
// NOTE: avoid using this for very long content unless you split it into
// multiple Pre blocks, because Telegram requires each message chunk to have
// balanced tags.
func Pre(s string) H {
	return H("<pre><code>" + html.EscapeString(s) + "</code></pre>")
}

// Link builds an HTML link.
func Link(text, url string) H {
	// Escape attribute; html.EscapeString also escapes quotes.
	return H(fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(url), html.EscapeString(text)))
}

// Mention links to a Telegram user ID.
func Mention(name string, userID int64) H {
	return Link(name, fmt.Sprintf("tg://user?id=%d", userID))
}

// JoinH joins safe HTML parts with sep.
func JoinH(sep string, parts ...H) H {
	if len(parts) == 0 {
		return ""
	}
	ss := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p.String()) == "" {
			continue
		}
		ss = append(ss, p.String())
	}
	return H(strings.Join(ss, sep))
}
