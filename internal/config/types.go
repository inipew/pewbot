package config

import (
	"bytes"
	"encoding/json"
)

type Config struct {
	Telegram TelegramConfig `json:"telegram"`
	Logging  LoggingConfig  `json:"logging"`
	Pprof    PprofConfig    `json:"pprof,omitempty"`

	// Scheduler controls trigger behavior (cron/interval/once).
	//
	// Legacy note:
	//   - Older configs also put execution settings under this block
	//     (workers/default_timeout/history_size/retry_max).
	//   - Those legacy fields are still accepted for backwards compatibility,
	//     but new configs should use task_engine instead.
	Scheduler SchedulerConfig `json:"scheduler"`

	// TaskEngine controls execution settings for scheduled tasks.
	// If omitted, execution settings fall back to legacy scheduler fields.
	TaskEngine *TaskEngineConfig `json:"task_engine,omitempty"`

	Notifier *NotifierConfig            `json:"notifier,omitempty"`
	Storage  *StorageConfig             `json:"storage,omitempty"`
	Plugins  map[string]PluginConfigRaw `json:"plugins"`
}

// TaskEngineConfig controls the task execution engine.
//
// All durations are Go duration strings (e.g. "500ms", "10s", "1m").
//
// Enabled is a pointer so we can distinguish "omitted" (default to scheduler.enabled)
// from an explicit false.
//
// Defaults (when fields are omitted/zero):
//   - enabled: scheduler.enabled
//   - workers: 2
//   - queue_size: 256
//   - default_timeout: "0s" (disabled)
//   - max_queue_delay: "0s" (disabled)
//   - history_size: 200
//   - retry_max: 3
type TaskEngineConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
	Workers int   `json:"workers,omitempty"`

	QueueSize int `json:"queue_size,omitempty"`

	// DefaultTimeout is a Go duration string (e.g. "10s", "1m").
	// Use "0s" to disable a global default timeout.
	DefaultTimeout string `json:"default_timeout,omitempty"`

	// MaxQueueDelay drops tasks that have been queued longer than this duration.
	// Use "0s" to disable stale queue dropping.
	MaxQueueDelay string `json:"max_queue_delay,omitempty"`

	HistorySize int `json:"history_size,omitempty"`
	RetryMax    int `json:"retry_max,omitempty"`
}

// NotifierConfig controls the async notification pipeline.
//
// All durations are Go duration strings (e.g. "500ms", "10s", "1m").
// If the whole section is omitted, the notifier defaults to enabled=true
// for backwards compatibility.
type NotifierConfig struct {
	Enabled         bool   `json:"enabled"`
	Workers         int    `json:"workers"`
	QueueSize       int    `json:"queue_size"`
	RatePerSec      int    `json:"rate_per_sec"`
	RetryMax        int    `json:"retry_max"`
	RetryBase       string `json:"retry_base"`
	RetryMaxDelay   string `json:"retry_max_delay"`
	DedupWindow     string `json:"dedup_window"`
	DedupMaxEntries int    `json:"dedup_max_entries"`
	PersistDedup    bool   `json:"persist_dedup,omitempty"`
}

// StorageConfig controls the optional persistence layer.
//
// Example:
//
//	"storage": { "driver": "file", "path": "./pewbot_store" }
type StorageConfig struct {
	Driver      string `json:"driver"`
	Path        string `json:"path"`
	BusyTimeout string `json:"busy_timeout,omitempty"` // Go duration string (sqlite)
}

// PprofConfig controls the optional pprof HTTP server.
//
// Security note:
//   - Prefer binding to localhost (e.g. "127.0.0.1:6060").
//   - If you bind to a non-loopback address, set a token or explicitly allow_insecure.
type PprofConfig struct {
	Enabled       bool   `json:"enabled"`
	Addr          string `json:"addr,omitempty"`   // default: "127.0.0.1:6060"
	Prefix        string `json:"prefix,omitempty"` // default: "/debug/pprof/"
	Token         string `json:"token,omitempty"`  // optional bearer token (do not log)
	AllowInsecure bool   `json:"allow_insecure,omitempty"`

	// Server timeouts (Go duration strings). WriteTimeout defaults to 0 (disabled)
	// so /profile (which can take 30s+) works reliably.
	ReadTimeout  string `json:"read_timeout,omitempty"`
	WriteTimeout string `json:"write_timeout,omitempty"`
	IdleTimeout  string `json:"idle_timeout,omitempty"`

	// Runtime profiling rates. Leave 0 to keep Go defaults.
	MutexProfileFraction int `json:"mutex_profile_fraction,omitempty"`
	BlockProfileRate     int `json:"block_profile_rate,omitempty"`
	MemProfileRate       int `json:"mem_profile_rate,omitempty"`
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

// SchedulerConfig controls the scheduler (trigger) service.
//
// Deprecated legacy fields:
//   - Workers, DefaultTimeout, HistorySize, RetryMax
//
// These fields used to configure execution. They are still accepted so older
// configs don't break under DisallowUnknownFields, but new configs should use
// task_engine instead.
type SchedulerConfig struct {
	Enabled bool `json:"enabled"`

	// Deprecated (legacy executor settings): use task_engine instead.
	Workers int `json:"workers"`
	// DefaultTimeout is a Go duration string (e.g. "10s", "1m").
	// Use "0s" to disable a global default timeout.
	DefaultTimeout string `json:"default_timeout"`
	HistorySize    int    `json:"history_size"`
	RetryMax       int    `json:"retry_max,omitempty"`

	// Trigger timezone.
	Timezone string `json:"timezone,omitempty"`
}

type PluginConfigRaw struct {
	Enabled bool `json:"enabled"`
	// Allow is an optional capability allowlist for this plugin.
	//
	// Notes:
	//   - This is NOT a security boundary (plugins are still in-process).
	//   - It is an operational guardrail: core will wrap selected ports
	//     (Scheduler/Notifier/Storage) and deny calls that don't match.
	//   - If omitted or empty, all capabilities are allowed (backwards compatible).
	Allow  []string        `json:"allow,omitempty"`
	Config json.RawMessage `json:"config,omitempty"`
}

// UnmarshalJSON disallows unknown fields to ensure removed legacy keys
// (e.g. "timeout") are caught early during config reload.
func (p *PluginConfigRaw) UnmarshalJSON(b []byte) error {
	type tmp struct {
		Enabled bool            `json:"enabled"`
		Allow   []string        `json:"allow,omitempty"`
		Config  json.RawMessage `json:"config,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var t tmp
	if err := dec.Decode(&t); err != nil {
		return err
	}
	*p = PluginConfigRaw{Enabled: t.Enabled, Allow: t.Allow, Config: t.Config}
	return nil
}
