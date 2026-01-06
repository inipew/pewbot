package pluginkit

import (
	"encoding/json"
	"fmt"
	"time"
)

// TimeoutsConfig standardizes common timeout knobs used by plugins.
//
// All fields accept Go duration strings (e.g. "10s", "2m", "1h", "2h30m").
//
// Recommended semantics:
//   - Command:   timeout for chat commands / callbacks handled by the plugin.
//   - Task:      timeout for background scheduled tasks (cron/interval jobs).
//   - Operation: timeout for a single internal operation (IO/network/system call) inside a command/task.
//
// JSON schema:
//
//	"timeouts": {
//	  "command": "15s",
//	  "task": "2m",
//	  "operation": "2s"
//	}
//
// Legacy fields like "job" / "request" are intentionally rejected.
type TimeoutsConfig struct {
	Command   string `json:"command,omitempty"`
	Task      string `json:"task,omitempty"`
	Operation string `json:"operation,omitempty"`
}

// UnmarshalJSON rejects legacy/unknown fields to avoid silent misconfiguration.
func (t *TimeoutsConfig) UnmarshalJSON(b []byte) error {
	// allow null / empty
	if len(b) == 0 || string(b) == "null" {
		*t = TimeoutsConfig{}
		return nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var out TimeoutsConfig
	for k, v := range m {
		switch k {
		case "command":
			_ = json.Unmarshal(v, &out.Command)
		case "task":
			_ = json.Unmarshal(v, &out.Task)
		case "operation":
			_ = json.Unmarshal(v, &out.Operation)
		case "job":
			return fmt.Errorf("timeouts.job is no longer supported; use timeouts.task")
		case "request":
			return fmt.Errorf("timeouts.request is no longer supported; use timeouts.operation")
		case "auto_task":
			return fmt.Errorf("timeouts.auto_task is no longer supported; use timeouts.task")
		default:
			return fmt.Errorf("unknown timeouts field %q (supported: command, task, operation)", k)
		}
	}
	*t = out
	return nil
}

// Validate validates non-empty duration strings and returns a contextual error.
// fieldPrefix should be something like "speedtest.timeouts".
func (t TimeoutsConfig) Validate(fieldPrefix string) error {
	if t.Command != "" {
		if _, err := time.ParseDuration(t.Command); err != nil {
			return fmt.Errorf("invalid %s.command: %w", fieldPrefix, err)
		}
	}
	if t.Task != "" {
		if _, err := time.ParseDuration(t.Task); err != nil {
			return fmt.Errorf("invalid %s.task: %w", fieldPrefix, err)
		}
	}
	if t.Operation != "" {
		if _, err := time.ParseDuration(t.Operation); err != nil {
			return fmt.Errorf("invalid %s.operation: %w", fieldPrefix, err)
		}
	}
	return nil
}

// CommandOr parses Command and returns it, or returns def when empty.
// If parsing fails, def is returned (validation should usually catch this earlier).
func (t TimeoutsConfig) CommandOr(def time.Duration) time.Duration {
	if t.Command == "" {
		return def
	}
	d, err := time.ParseDuration(t.Command)
	if err != nil {
		return def
	}
	return d
}

// TaskOr parses Task and returns it, or returns def when empty.
// If parsing fails, def is returned (validation should usually catch this earlier).
func (t TimeoutsConfig) TaskOr(def time.Duration) time.Duration {
	if t.Task == "" {
		return def
	}
	d, err := time.ParseDuration(t.Task)
	if err != nil {
		return def
	}
	return d
}

// OperationOr parses Operation and returns it, or returns def when empty.
// If parsing fails, def is returned (validation should usually catch this earlier).
func (t TimeoutsConfig) OperationOr(def time.Duration) time.Duration {
	if t.Operation == "" {
		return def
	}
	d, err := time.ParseDuration(t.Operation)
	if err != nil {
		return def
	}
	return d
}
