package core

import (
	"fmt"
	"time"

	"pewbot/internal/services/notify"
)

// mapNotifierConfig maps core.Config (JSON) into the runtime notify.Config (parsed durations).
//
// Backwards compatibility:
//   - If cfg.notifier is omitted, we default to enabled=true with sensible defaults.
func mapNotifierConfig(cfg *Config) (notify.Config, error) {
	// Defaults (match the target design proposal).
	out := notify.Config{
		Enabled:         true,
		Workers:         2,
		QueueSize:       512,
		RatePerSec:      3,
		RetryMax:        3,
		RetryBase:       500 * time.Millisecond,
		RetryMaxDelay:   10 * time.Second,
		DedupWindow:     1 * time.Minute,
		DedupMaxEntries: 2000,
		PersistDedup:    false,
	}

	if cfg == nil || cfg.Notifier == nil {
		return out, nil
	}
	n := cfg.Notifier
	out.Enabled = n.Enabled
	out.PersistDedup = n.PersistDedup
	if n.Workers != 0 {
		out.Workers = n.Workers
	}
	if n.QueueSize != 0 {
		out.QueueSize = n.QueueSize
	}
	if n.RatePerSec != 0 {
		out.RatePerSec = n.RatePerSec
	}
	if n.RetryMax != 0 {
		out.RetryMax = n.RetryMax
	}

	// Durations.
	var err error
	out.RetryBase, err = parseDurationOrDefault("notifier.retry_base", n.RetryBase, out.RetryBase)
	if err != nil {
		return notify.Config{}, err
	}
	out.RetryMaxDelay, err = parseDurationOrDefault("notifier.retry_max_delay", n.RetryMaxDelay, out.RetryMaxDelay)
	if err != nil {
		return notify.Config{}, err
	}
	out.DedupWindow, err = parseDurationOrDefault("notifier.dedup_window", n.DedupWindow, out.DedupWindow)
	if err != nil {
		return notify.Config{}, err
	}
	if n.DedupMaxEntries != 0 {
		out.DedupMaxEntries = n.DedupMaxEntries
	}
	out.PersistDedup = n.PersistDedup

	// Bounds.
	if out.Workers < 0 {
		return notify.Config{}, fmt.Errorf("notifier.workers must be >= 0")
	}
	if out.QueueSize < 0 {
		return notify.Config{}, fmt.Errorf("notifier.queue_size must be >= 0")
	}
	if out.RatePerSec < 0 {
		return notify.Config{}, fmt.Errorf("notifier.rate_per_sec must be >= 0")
	}
	if out.RetryMax < 0 {
		return notify.Config{}, fmt.Errorf("notifier.retry_max must be >= 0")
	}
	if out.DedupMaxEntries < 0 {
		return notify.Config{}, fmt.Errorf("notifier.dedup_max_entries must be >= 0")
	}

	return out, nil
}
