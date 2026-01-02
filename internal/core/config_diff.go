package core

import (
	"log/slog"
	"reflect"
	"sort"
	"strings"
)

// SummarizeConfigChange returns (1) a compact list of changed sections,
// (2) safe structured attrs for logging (never includes secrets like tokens),
// and (3) a list of plugin names that changed (enable/timeout/config).
func SummarizeConfigChange(oldCfg, newCfg *Config) ([]string, []slog.Attr, []string) {
	if oldCfg == nil {
		oldCfg = &Config{}
	}
	if newCfg == nil {
		newCfg = &Config{}
	}

	changed := make([]string, 0, 5)
	attrs := make([]slog.Attr, 0, 16)

	// Telegram (never log token)
	if oldCfg.Telegram.PollTimeoutSec != newCfg.Telegram.PollTimeoutSec ||
		!reflect.DeepEqual(oldCfg.Telegram.OwnerUserIDs, newCfg.Telegram.OwnerUserIDs) ||
		strings.TrimSpace(oldCfg.Telegram.GroupLog) != strings.TrimSpace(newCfg.Telegram.GroupLog) {
		changed = append(changed, "telegram")
		attrs = append(attrs,
			slog.Int("telegram.poll_timeout_sec", newCfg.Telegram.PollTimeoutSec),
			slog.Int("telegram.owner_count", len(newCfg.Telegram.OwnerUserIDs)),
			slog.Bool("telegram.group_log_set", strings.TrimSpace(newCfg.Telegram.GroupLog) != ""),
		)
	}

	// Logging
	if oldCfg.Logging.Level != newCfg.Logging.Level ||
		oldCfg.Logging.Console != newCfg.Logging.Console ||
		oldCfg.Logging.File.Enabled != newCfg.Logging.File.Enabled ||
		strings.TrimSpace(oldCfg.Logging.File.Path) != strings.TrimSpace(newCfg.Logging.File.Path) ||
		oldCfg.Logging.Telegram.Enabled != newCfg.Logging.Telegram.Enabled ||
		oldCfg.Logging.Telegram.ThreadID != newCfg.Logging.Telegram.ThreadID ||
		oldCfg.Logging.Telegram.MinLevel != newCfg.Logging.Telegram.MinLevel ||
		oldCfg.Logging.Telegram.RatePerSec != newCfg.Logging.Telegram.RatePerSec {
		changed = append(changed, "logging")
		attrs = append(attrs,
			slog.String("logging.level", newCfg.Logging.Level),
			slog.Bool("logging.console", newCfg.Logging.Console),
			slog.Bool("logging.file_enabled", newCfg.Logging.File.Enabled),
			slog.Bool("logging.telegram_enabled", newCfg.Logging.Telegram.Enabled),
		)
	}

	// Scheduler
	if !reflect.DeepEqual(oldCfg.Scheduler, newCfg.Scheduler) {
		changed = append(changed, "scheduler")
		attrs = append(attrs,
			slog.Bool("scheduler.enabled", newCfg.Scheduler.Enabled),
			slog.Int("scheduler.workers", newCfg.Scheduler.Workers),
			slog.String("scheduler.timezone", newCfg.Scheduler.Timezone),
			slog.Int("scheduler.retry_max", newCfg.Scheduler.RetryMax),
		)
	}

	// Broadcaster
	if !reflect.DeepEqual(oldCfg.Broadcaster, newCfg.Broadcaster) {
		changed = append(changed, "broadcaster")
		attrs = append(attrs,
			slog.Bool("broadcaster.enabled", newCfg.Broadcaster.Enabled),
			slog.Int("broadcaster.workers", newCfg.Broadcaster.Workers),
			slog.Int("broadcaster.rps", newCfg.Broadcaster.RatePerSec),
			slog.Int("broadcaster.retry_max", newCfg.Broadcaster.RetryMax),
		)
	}

	// Plugins (summarize only; details at debug)
	pluginChanged := diffPlugins(oldCfg.Plugins, newCfg.Plugins)
	if len(pluginChanged) > 0 {
		changed = append(changed, "plugins")
		attrs = append(attrs,
			slog.Int("plugins.changed_count", len(pluginChanged)),
			slog.Int("plugins.enabled_count", countEnabled(newCfg.Plugins)),
		)
	}

	sort.Strings(changed)
	return changed, attrs, pluginChanged
}

func countEnabled(m map[string]PluginConfigRaw) int {
	if len(m) == 0 {
		return 0
	}
	n := 0
	for _, v := range m {
		if v.Enabled {
			n++
		}
	}
	return n
}

func diffPlugins(oldM, newM map[string]PluginConfigRaw) []string {
	if oldM == nil {
		oldM = map[string]PluginConfigRaw{}
	}
	if newM == nil {
		newM = map[string]PluginConfigRaw{}
	}

	set := map[string]struct{}{}
	for k := range oldM {
		set[k] = struct{}{}
	}
	for k := range newM {
		set[k] = struct{}{}
	}

	out := make([]string, 0, len(set))
	for name := range set {
		o := oldM[name]
		n := newM[name]
		if o.Enabled != n.Enabled {
			out = append(out, name)
			continue
		}
		if strings.TrimSpace(o.Timeout) != strings.TrimSpace(n.Timeout) {
			out = append(out, name)
			continue
		}
		if canonicalHashJSON(o.Config) != canonicalHashJSON(n.Config) {
			out = append(out, name)
			continue
		}
	}
	sort.Strings(out)
	return out
}
