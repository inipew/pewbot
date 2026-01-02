package core

import sch "pewbot/internal/services/scheduler"

// Re-export scheduler types for plugin SDK (plugins cannot import internal/services/scheduler).
type TaskOptions = sch.TaskOptions
type Snapshot = sch.Snapshot
type ScheduleInfo = sch.ScheduleInfo

// Schedule parsing helpers (re-exported for plugins).
type ScheduleKind = sch.SpecKind
type ParsedSchedule = sch.ParsedSpec

const (
	ScheduleCron     = sch.SpecCron
	ScheduleInterval = sch.SpecInterval
)

func ParseSchedule(raw string) (ParsedSchedule, error) {
	return sch.ParseSchedule(raw)
}

const (
	OverlapAllow         = sch.OverlapAllow
	OverlapSkipIfRunning = sch.OverlapSkipIfRunning
)
