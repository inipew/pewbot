package router

import (
	"pewbot/internal/config"
	"pewbot/internal/plugin/ops"
	"pewbot/internal/runtime/supervisor"
	"pewbot/internal/task/scheduler"
)

// ---- Config ----

type Config = config.Config

type ConfigManager = config.ConfigManager

// ---- Runtime ----

type Supervisor = supervisor.Supervisor

var NewSupervisor = supervisor.NewSupervisor

var WithLogger = supervisor.WithLogger

var WithCancelOnError = supervisor.WithCancelOnError

// ---- Restart helpers (for resilient worker loops) ----

type RestartOption = supervisor.RestartOption

var WithRestartBackoff = supervisor.WithRestartBackoff

var WithMaxRestarts = supervisor.WithMaxRestarts

var WithFatalOnFinalError = supervisor.WithFatalOnFinalError

var WithPublishFirstError = supervisor.WithPublishFirstError

var WithStopOnCleanExit = supervisor.WithStopOnCleanExit

// ---- Task/scheduler operational types ----

type TaskOptions = scheduler.TaskOptions

type Snapshot = scheduler.Snapshot

type OverlapPolicy = scheduler.OverlapPolicy

const (
	OverlapAllow         = scheduler.OverlapAllow
	OverlapSkipIfRunning = scheduler.OverlapSkipIfRunning
)

// ---- Plugin operational types (no import cycle) ----

type PluginsSnapshot = ops.PluginsSnapshot

type PluginStatus = ops.PluginStatus

type PluginHealthResult = ops.PluginHealthResult

// ---- Schedule parsing (shared between router & plugins) ----

type ScheduleKind = scheduler.SpecKind

type ParsedSchedule = scheduler.ParsedSpec

const (
	ScheduleCron     = scheduler.SpecCron
	ScheduleInterval = scheduler.SpecInterval
)

func ParseSchedule(raw string) (ParsedSchedule, error) {
	return scheduler.ParseSchedule(raw)
}
