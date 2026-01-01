package core

import (
	"encoding/json"
	"time"
)

type Config struct {
	Telegram    TelegramConfig             `json:"telegram"`
	Logging     LoggingConfig              `json:"logging"`
	Scheduler   SchedulerConfig            `json:"scheduler"`
	Broadcaster BroadcasterConfig          `json:"broadcaster"`
	Plugins     map[string]PluginConfigRaw `json:"plugins"`
}

type TelegramConfig struct {
	Token          string  `json:"token"`
	OwnerUserIDs   []int64 `json:"owner_user_ids"`
	GroupLog       string  `json:"group_log"`
	PollTimeoutSec int     `json:"poll_timeout_sec"`
}

type LoggingConfig struct {
	Level   string           `json:"level"`
	Console bool             `json:"console"`
	File    LoggingFile      `json:"file"`
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
	Enabled          bool   `json:"enabled"`
	Workers          int    `json:"workers"`
	DefaultTimeoutMS int    `json:"default_timeout_ms"`
	HistorySize      int    `json:"history_size"`
	Timezone         string `json:"timezone,omitempty"`
}

type BroadcasterConfig struct {
	Enabled    bool `json:"enabled"`
	Workers    int  `json:"workers"`
	RatePerSec int  `json:"rate_per_sec"`
	RetryMax   int  `json:"retry_max"`
}

type PluginConfigRaw struct {
	Enabled bool            `json:"enabled"`
	Timeout string          `json:"timeout,omitempty"`
	Config  json.RawMessage `json:"config,omitempty"`
}

func (p PluginConfigRaw) TimeoutDuration() (time.Duration, bool) {
	if p.Timeout == "" {
		return 0, false
	}
	d, err := time.ParseDuration(p.Timeout)
	if err != nil {
		return 0, false
	}
	return d, true
}
