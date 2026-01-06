package tgui

import (
	tele "gopkg.in/telebot.v4"
)

// Inline is a small builder for inline keyboards (ReplyMarkup).
// It stores rows as tele.Row ([]tele.Btn) and applies them via ReplyMarkup.Inline().
type Inline struct {
	rm   *tele.ReplyMarkup
	rows []tele.Row
}

func NewInline() *Inline {
	return &Inline{rm: &tele.ReplyMarkup{}}
}

// Row appends a new row (buttons) to the inline keyboard.
func (i *Inline) Row(btn ...tele.Btn) *Inline {
	i.rows = append(i.rows, i.rm.Row(btn...))
	return i
}

// Markup returns underlying reply markup.
func (i *Inline) Markup() *tele.ReplyMarkup {
	if i == nil {
		return nil
	}
	// Apply rows lazily to avoid rebuilding the markup on every Row() call.
	if i.rm != nil {
		i.rm.Inline(i.rows...)
	}
	return i.rm
}

// Btn creates a callback button with raw callback_data (we do NOT encode it).
// Use pkg/tgui/callback.go helpers to build "plugin:action:payload" safely.
func Btn(text, data string) tele.Btn {
	return tele.Btn{Text: text, Data: data}
}

// URLBtn creates a URL button.
func URLBtn(text, url string) tele.Btn {
	return tele.Btn{Text: text, URL: url}
}

// ActionBtn creates a callback button using ActionData semantics.
// See pkg/tgui/callback.go for payload rules.
func ActionBtn(text, plugin, action string, payload any) tele.Btn {
	return tele.Btn{Text: text, Data: MustActionData(plugin, action, payload)}
}

// ActionBtnWithStore is like ActionBtn, but uses store as a fallback if the
// payload makes the callback_data exceed Telegram's 64-byte limit.
//
// If it still fails, it falls back to a payload-less callback.
func ActionBtnWithStore(text, plugin, action string, payload any, store *TokenStore) tele.Btn {
	data, err := ActionDataWithStore(plugin, action, payload, store)
	if err != nil {
		data = Data(plugin, action, "")
	}
	return tele.Btn{Text: text, Data: data}
}

// Grid2 splits buttons into 2 columns and returns a ready ReplyMarkup.
func Grid2(buttons []tele.Btn) *tele.ReplyMarkup {
	rm := &tele.ReplyMarkup{}
	rows := rm.Split(2, buttons)
	rm.Inline(rows...)
	return rm
}

// Grid splits buttons into N columns and returns a ready ReplyMarkup.
func Grid(cols int, buttons []tele.Btn) *tele.ReplyMarkup {
	if cols <= 0 {
		cols = 2
	}
	rm := &tele.ReplyMarkup{}
	rows := rm.Split(cols, buttons)
	rm.Inline(rows...)
	return rm
}
