package core

import (
	"bytes"
	"encoding/json"
)

type Config struct {
	Telegram  TelegramConfig             `json:"telegram"`
	Logging   LoggingConfig              `json:"logging"`
	Scheduler SchedulerConfig            `json:"scheduler"`
	Plugins   map[string]PluginConfigRaw `json:"plugins"`
}

type TelegramConfig struct {
	Token        string  `json:"token"`
	OwnerUserIDs []int64 `json:"owner_user_ids"`
	GroupLog     string  `json:"group_log"`
	// PollTimeout is a Go duration string (e.g. "10s", "2m").
	PollTimeout string `json:"poll_timeout"`
}

type LoggingConfig struct {
	Level    string          `json:"level"`
	Console  bool            `json:"console"`
	File     LoggingFile     `json:"file"`
	Telegram LoggingTelegram `json:"telegram"`
}
type LoggingFile struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path"`
}
type LoggingTelegram struct {
	Enabled    bool   `json:"enabled"`
	ThreadID   int    `json:"thread_id"`
	MinLevel   string `json:"min_level"`
	RatePerSec int    `json:"rate_per_sec"`
}

type SchedulerConfig struct {
	Enabled bool `json:"enabled"`
	Workers int  `json:"workers"`
	// DefaultTimeout is a Go duration string (e.g. "10s", "1m").
	// Use "0s" to disable a global default timeout.
	DefaultTimeout string `json:"default_timeout"`
	HistorySize    int    `json:"history_size"`
	Timezone       string `json:"timezone,omitempty"`
	RetryMax       int    `json:"retry_max,omitempty"`
}
type PluginConfigRaw struct {
	Enabled bool            `json:"enabled"`
	Config  json.RawMessage `json:"config,omitempty"`
}

// UnmarshalJSON disallows unknown fields to ensure removed legacy keys
// (e.g. "timeout") are caught early during config reload.
func (p *PluginConfigRaw) UnmarshalJSON(b []byte) error {
	type tmp struct {
		Enabled bool            `json:"enabled"`
		Config  json.RawMessage `json:"config,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var t tmp
	if err := dec.Decode(&t); err != nil {
		return err
	}
	*p = PluginConfigRaw{Enabled: t.Enabled, Config: t.Config}
	return nil
}
