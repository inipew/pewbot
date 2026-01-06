package config

import (
	logx "pewbot/pkg/logx"
	"reflect"
	"sort"
	"strings"
)

// SummarizeConfigChange returns (1) a compact list of changed sections,
// (2) safe structured attrs for logging (never includes secrets like tokens),
// and (3) a list of plugin names that changed (enable/config).
func SummarizeConfigChange(oldCfg, newCfg *Config) ([]string, []logx.Field, []string) {
	if oldCfg == nil {
		oldCfg = &Config{}
	}
	if newCfg == nil {
		newCfg = &Config{}
	}

	changed := make([]string, 0, 6)
	attrs := make([]logx.Field, 0, 20)

	// Telegram (never log token)
	if strings.TrimSpace(oldCfg.Telegram.PollTimeout) != strings.TrimSpace(newCfg.Telegram.PollTimeout) ||
		!reflect.DeepEqual(oldCfg.Telegram.OwnerUserIDs, newCfg.Telegram.OwnerUserIDs) ||
		strings.TrimSpace(oldCfg.Telegram.GroupLog) != strings.TrimSpace(newCfg.Telegram.GroupLog) {
		changed = append(changed, "telegram")
		attrs = append(attrs,
			logx.String("telegram.poll_timeout", strings.TrimSpace(newCfg.Telegram.PollTimeout)),
			logx.Int("telegram.owner_count", len(newCfg.Telegram.OwnerUserIDs)),
			logx.Bool("telegram.group_log_set", strings.TrimSpace(newCfg.Telegram.GroupLog) != ""),
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
			logx.String("logx.level", newCfg.Logging.Level),
			logx.Bool("logx.console", newCfg.Logging.Console),
			logx.Bool("logx.file_enabled", newCfg.Logging.File.Enabled),
			logx.Bool("logx.telegram_enabled", newCfg.Logging.Telegram.Enabled),
		)
	}

	// Pprof (never log token)
	if oldCfg.Pprof.Enabled != newCfg.Pprof.Enabled ||
		strings.TrimSpace(oldCfg.Pprof.Addr) != strings.TrimSpace(newCfg.Pprof.Addr) ||
		strings.TrimSpace(oldCfg.Pprof.Prefix) != strings.TrimSpace(newCfg.Pprof.Prefix) ||
		oldCfg.Pprof.AllowInsecure != newCfg.Pprof.AllowInsecure ||
		strings.TrimSpace(oldCfg.Pprof.ReadTimeout) != strings.TrimSpace(newCfg.Pprof.ReadTimeout) ||
		strings.TrimSpace(oldCfg.Pprof.WriteTimeout) != strings.TrimSpace(newCfg.Pprof.WriteTimeout) ||
		strings.TrimSpace(oldCfg.Pprof.IdleTimeout) != strings.TrimSpace(newCfg.Pprof.IdleTimeout) ||
		oldCfg.Pprof.MutexProfileFraction != newCfg.Pprof.MutexProfileFraction ||
		oldCfg.Pprof.BlockProfileRate != newCfg.Pprof.BlockProfileRate ||
		oldCfg.Pprof.MemProfileRate != newCfg.Pprof.MemProfileRate ||
		(strings.TrimSpace(oldCfg.Pprof.Token) != "") != (strings.TrimSpace(newCfg.Pprof.Token) != "") {
		changed = append(changed, "pprof")
		attrs = append(attrs,
			logx.Bool("pprof.enabled", newCfg.Pprof.Enabled),
			logx.String("pprof.addr", strings.TrimSpace(newCfg.Pprof.Addr)),
			logx.String("pprof.prefix", strings.TrimSpace(newCfg.Pprof.Prefix)),
			logx.Bool("pprof.token_set", strings.TrimSpace(newCfg.Pprof.Token) != ""),
			logx.Bool("pprof.allow_insecure", newCfg.Pprof.AllowInsecure),
		)
	}

	// Scheduler (triggers). Keep legacy fields accepted for backwards compatibility.
	// We still surface legacy values in the summary because they may be used as a fallback
	// when task_engine is omitted.
	schedTriggerChanged := oldCfg.Scheduler.Enabled != newCfg.Scheduler.Enabled ||
		strings.TrimSpace(oldCfg.Scheduler.Timezone) != strings.TrimSpace(newCfg.Scheduler.Timezone)
	schedLegacyChanged := oldCfg.Scheduler.Workers != newCfg.Scheduler.Workers ||
		strings.TrimSpace(oldCfg.Scheduler.DefaultTimeout) != strings.TrimSpace(newCfg.Scheduler.DefaultTimeout) ||
		oldCfg.Scheduler.HistorySize != newCfg.Scheduler.HistorySize ||
		oldCfg.Scheduler.RetryMax != newCfg.Scheduler.RetryMax
	if schedTriggerChanged || schedLegacyChanged {
		changed = append(changed, "scheduler")
		attrs = append(attrs,
			logx.Bool("scheduler.enabled", newCfg.Scheduler.Enabled),
			logx.String("scheduler.timezone", strings.TrimSpace(newCfg.Scheduler.Timezone)),
			logx.Bool("scheduler.legacy_changed", schedLegacyChanged),
		)
		if schedLegacyChanged {
			attrs = append(attrs,
				logx.Int("scheduler.workers_legacy", newCfg.Scheduler.Workers),
				logx.String("scheduler.default_timeout_legacy", strings.TrimSpace(newCfg.Scheduler.DefaultTimeout)),
				logx.Int("scheduler.history_size_legacy", newCfg.Scheduler.HistorySize),
				logx.Int("scheduler.retry_max_legacy", newCfg.Scheduler.RetryMax),
			)
		}
	}

	// Task engine (executor)
	oTE := derefTaskEngine(oldCfg.TaskEngine)
	nTE := derefTaskEngine(newCfg.TaskEngine)
	oPresent := oldCfg.TaskEngine != nil
	nPresent := newCfg.TaskEngine != nil
	if oPresent != nPresent || !reflect.DeepEqual(oTE, nTE) {
		changed = append(changed, "task_engine")

		enabledEffective := newCfg.Scheduler.Enabled
		enabledSet := false
		if newCfg.TaskEngine != nil && newCfg.TaskEngine.Enabled != nil {
			enabledSet = true
			enabledEffective = *newCfg.TaskEngine.Enabled
		}

		attrs = append(attrs,
			logx.Bool("task_engine.present", nPresent),
			logx.Bool("task_engine.enabled", enabledEffective),
			logx.Bool("task_engine.enabled_set", enabledSet),
			logx.Int("task_engine.workers", nTE.Workers),
			logx.Int("task_engine.queue_size", nTE.QueueSize),
			logx.String("task_engine.default_timeout", strings.TrimSpace(nTE.DefaultTimeout)),
			logx.String("task_engine.max_queue_delay", strings.TrimSpace(nTE.MaxQueueDelay)),
			logx.Int("task_engine.history_size", nTE.HistorySize),
			logx.Int("task_engine.retry_max", nTE.RetryMax),
		)
	}

	// Notifier (async pipeline)
	// Note: section may be nil (omitted). Treat nil as runtime defaults for a more accurate summary.
	defN := &NotifierConfig{
		Enabled:         true,
		Workers:         2,
		QueueSize:       512,
		RatePerSec:      3,
		RetryMax:        3,
		RetryBase:       "500ms",
		RetryMaxDelay:   "10s",
		DedupWindow:     "1m",
		DedupMaxEntries: 2000,
		PersistDedup:    false,
	}
	oldN := oldCfg.Notifier
	newN := newCfg.Notifier
	if oldN == nil {
		oldN = defN
	}
	if newN == nil {
		newN = defN
	}
	if !reflect.DeepEqual(*oldN, *newN) {
		changed = append(changed, "notifier")
		attrs = append(attrs,
			logx.Bool("notifier.enabled", newN.Enabled),
			logx.Int("notifier.workers", newN.Workers),
			logx.Int("notifier.queue_size", newN.QueueSize),
			logx.Int("notifier.rate_per_sec", newN.RatePerSec),
			logx.Int("notifier.retry_max", newN.RetryMax),
			logx.Bool("notifier.persist_dedup", newN.PersistDedup),
		)
	}

	// Storage (persistence)
	oldS := oldCfg.Storage
	newS := newCfg.Storage
	// Nil means disabled.
	var oDriver, nDriver, oBusy, nBusy string
	var oPathSet, nPathSet bool
	if oldS != nil {
		oDriver = strings.TrimSpace(oldS.Driver)
		oBusy = strings.TrimSpace(oldS.BusyTimeout)
		oPathSet = strings.TrimSpace(oldS.Path) != ""
	}
	if newS != nil {
		nDriver = strings.TrimSpace(newS.Driver)
		nBusy = strings.TrimSpace(newS.BusyTimeout)
		nPathSet = strings.TrimSpace(newS.Path) != ""
	}
	if oDriver != nDriver || oBusy != nBusy || oPathSet != nPathSet {
		changed = append(changed, "storage")
		attrs = append(attrs,
			logx.String("storage.driver", nDriver),
			logx.Bool("storage.path_set", nPathSet),
			logx.String("storage.busy_timeout", nBusy),
		)
	}

	// Plugins (summarize only; details at debug)
	pluginChanged := diffPlugins(oldCfg.Plugins, newCfg.Plugins)
	if len(pluginChanged) > 0 {
		changed = append(changed, "plugins")
		attrs = append(attrs,
			logx.Int("plugins.changed_count", len(pluginChanged)),
			logx.Int("plugins.enabled_count", countEnabled(newCfg.Plugins)),
		)
	}

	sort.Strings(changed)
	return changed, attrs, pluginChanged
}

func derefTaskEngine(te *TaskEngineConfig) TaskEngineConfig {
	if te == nil {
		return TaskEngineConfig{}
	}
	return *te
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
		if canonicalHashJSON(o.Config) != canonicalHashJSON(n.Config) {
			out = append(out, name)
			continue
		}
	}
	sort.Strings(out)
	return out
}
