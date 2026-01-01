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
	i.rm.Inline(i.rows...)
	return i
}

// Markup returns underlying reply markup.
func (i *Inline) Markup() *tele.ReplyMarkup { return i.rm }

// Btn creates a callback button with raw callback_data (we do NOT encode it).
// Use pkg/tgui/callback.go helpers to build "plugin:action:payload" safely.
func Btn(text, data string) tele.Btn {
	return tele.Btn{Text: text, Data: data}
}

// URLBtn creates a URL button.
func URLBtn(text, url string) tele.Btn {
	return tele.Btn{Text: text, URL: url}
}

// Grid2 splits buttons into 2 columns and returns a ready ReplyMarkup.
func Grid2(buttons []tele.Btn) *tele.ReplyMarkup {
	rm := &tele.ReplyMarkup{}
	rows := rm.Split(2, buttons)
	rm.Inline(rows...)
	return rm
}
