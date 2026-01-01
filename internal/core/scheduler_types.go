package core

import sch "pewbot/internal/services/scheduler"

// Re-export scheduler types for plugin SDK (plugins cannot import internal/services/scheduler).
type TaskOptions = sch.TaskOptions
type Snapshot = sch.Snapshot
type ScheduleInfo = sch.ScheduleInfo

const (
	OverlapAllow         = sch.OverlapAllow
	OverlapSkipIfRunning = sch.OverlapSkipIfRunning
)
