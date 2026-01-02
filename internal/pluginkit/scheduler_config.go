package pluginkit

// SchedulerTaskConfig is a small, plugin-local scheduling configuration that can be reused
// across plugins to keep scheduler-related settings consistent.
//
// It configures *one* scheduled task inside a plugin.
//
// Recommended JSON schema:
//
//	"scheduler": {
//	  "enabled": true,
//	  "task_name": "auto_recover",
//	  "schedule": "30s"
//	}
//
// Schedule supports the same formats as core.ParseSchedule:
// cron (5/6-field), "@every 55m", duration ("55m"), or HH:MM ("02:30").
type SchedulerTaskConfig struct {
	Enabled  bool   `json:"enabled"`
	TaskName string `json:"task_name,omitempty"`
	Schedule string `json:"schedule,omitempty"`
}

// NameOr returns TaskName if set, otherwise def.
func (c SchedulerTaskConfig) NameOr(def string) string {
	if c.TaskName != "" {
		return c.TaskName
	}
	return def
}

// Active reports whether the task should be scheduled.
func (c SchedulerTaskConfig) Active() bool { return c.Enabled && c.Schedule != "" }
