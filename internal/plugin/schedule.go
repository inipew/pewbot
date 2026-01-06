package plugin

import "pewbot/internal/transport/telegram/router"

// Schedule parsing helpers.
// These are used by pluginkit and built-in plugins to validate config.

type ScheduleKind = router.ScheduleKind

type ParsedSchedule = router.ParsedSchedule

const (
	ScheduleCron     = router.ScheduleCron
	ScheduleInterval = router.ScheduleInterval
)

func ParseSchedule(raw string) (ParsedSchedule, error) {
	return router.ParseSchedule(raw)
}
