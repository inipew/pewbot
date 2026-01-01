package tgui

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

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
