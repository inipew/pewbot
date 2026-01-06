package tgui

import "errors"

// MaxCallbackDataLen is Telegram's callback_data size limit in bytes.
// NOTE: This is the length of the full string: "plugin:action:payload".
const MaxCallbackDataLen = 64

var ErrCallbackDataTooLong = errors.New("tgui: callback_data too long")
