package tgui

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// RawPayload marks a callback payload as already-encoded for Telegram.
// Use when you have a base64url payload or a short safe token.
type RawPayload string

// PackString encodes a plain string using base64url (no padding), suitable for
// callback payloads (it avoids ':' and other reserved chars).
func PackString(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// UnpackString decodes a base64url string produced by PackString.
func UnpackString(payload string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ActionData is a "smart" callback data builder.
//
// Payload handling:
//   - nil: no payload part
//   - RawPayload: used as-is
//   - string: base64url encoded (PackString)
//   - other types: JSON marshaled then base64url encoded (PackJSON)
func ActionData(plugin, action string, payload any) (string, error) {
	if payload == nil {
		out := Data(plugin, action, "")
		if len(out) > MaxCallbackDataLen {
			return "", ErrCallbackDataTooLong
		}
		return out, nil
	}
	switch v := payload.(type) {
	case RawPayload:
		out := Data(plugin, action, string(v))
		if len(out) > MaxCallbackDataLen {
			return "", ErrCallbackDataTooLong
		}
		return out, nil
	case string:
		out := Data(plugin, action, PackString(v))
		if len(out) > MaxCallbackDataLen {
			return "", ErrCallbackDataTooLong
		}
		return out, nil
	default:
		p, err := PackJSON(payload)
		if err != nil {
			return "", err
		}
		out := Data(plugin, action, p)
		if len(out) > MaxCallbackDataLen {
			return "", ErrCallbackDataTooLong
		}
		return out, nil
	}
}

// MustActionData is like ActionData but returns a callback without payload on error.
func MustActionData(plugin, action string, payload any) string {
	s, err := ActionData(plugin, action, payload)
	if err != nil {
		return Data(plugin, action, "")
	}
	return s
}

// ActionDataWithStore is like ActionData, but if the produced callback_data is
// too long, it stores the payload in store and uses the returned token as
// payload instead.
//
// This helps when the payload is large (e.g. long text, structured JSON), while
// respecting Telegram's 64-byte callback_data limit.
func ActionDataWithStore(plugin, action string, payload any, store *TokenStore) (string, error) {
	out, err := ActionData(plugin, action, payload)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, ErrCallbackDataTooLong) || store == nil {
		return "", err
	}

	// Fallback: store the payload and reference it by a short token.
	switch v := payload.(type) {
	case nil:
		out := Data(plugin, action, "")
		if len(out) > MaxCallbackDataLen {
			return "", ErrCallbackDataTooLong
		}
		return out, nil
	case RawPayload:
		// already-encoded; cannot safely shrink it
		return "", ErrCallbackDataTooLong
	case string:
		tok := store.PutString(v)
		out := Data(plugin, action, tok)
		if len(out) > MaxCallbackDataLen {
			return "", ErrCallbackDataTooLong
		}
		return out, nil
	default:
		tok, e := store.PutJSON(payload)
		if e != nil {
			return "", e
		}
		out := Data(plugin, action, tok)
		if len(out) > MaxCallbackDataLen {
			return "", ErrCallbackDataTooLong
		}
		return out, nil
	}
}

// Data formats inline callback data as "plugin:action:payload".
// Payload is kept as-is (no escaping). If you pass structured payload,
// prefer PackJSON.
func Data(plugin, action, payload string) string {
	plugin = strings.TrimSpace(plugin)
	action = strings.TrimSpace(action)
	if payload == "" {
		return plugin + ":" + action
	}
	return plugin + ":" + action + ":" + payload
}

// PackJSON marshals v to JSON then Base64URL encodes it (no padding),
// suitable for the payload part of "plugin:action:payload".
func PackJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// MustPackJSON is like PackJSON but returns empty string on error.
func MustPackJSON(v any) string {
	s, _ := PackJSON(v)
	return s
}

// UnpackJSON decodes base64url payload then unmarshals into v.
func UnpackJSON(payload string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
