package app

import (
	"pewbot/internal/config"
	"pewbot/internal/plugin"
	"pewbot/internal/runtime/supervisor"
	"pewbot/internal/transport/telegram/router"
	"time"
)

// ---- Config ----

type Config = config.Config

type ConfigManager = config.ConfigManager

var NewConfigManager = config.NewConfigManager

// SummarizeConfigChange produces a safe, structured summary of config diffs.
// Kept here as a compatibility alias so internal/app doesn't need to import internal/config directly.
var SummarizeConfigChange = config.SummarizeConfigChange

func parseDurationField(path, raw string) (time.Duration, error) {
	return config.ParseDurationField(path, raw)
}

func parseDurationOrDefault(path, raw string, def time.Duration) (time.Duration, error) {
	return config.ParseDurationOrDefault(path, raw, def)
}

// ---- Runtime ----

type Supervisor = supervisor.Supervisor

type SupervisorOption = supervisor.SupervisorOption

type SupervisorCounters = supervisor.SupervisorCounters

type SupervisorRegistry = router.SupervisorRegistry

var NewSupervisor = supervisor.NewSupervisor

var NewSupervisorRegistry = router.NewSupervisorRegistry

var WithLogger = supervisor.WithLogger

var WithCancelOnError = supervisor.WithCancelOnError

// ---- Router ----

type Services = router.Services

type CommandManager = router.CommandManager

var NewCommandManager = router.NewCommandManager

// ---- Plugin ----

type PluginManager = plugin.PluginManager

type PluginDeps = plugin.PluginDeps

var NewPluginManager = plugin.NewPluginManager
