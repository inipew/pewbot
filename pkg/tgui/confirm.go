package tgui

import tele "gopkg.in/telebot.v4"

// ConfirmInline builds a simple 2-button confirm keyboard.
func ConfirmInline(yes, no tele.Btn) *Inline {
	return NewInline().Row(yes, no)
}
