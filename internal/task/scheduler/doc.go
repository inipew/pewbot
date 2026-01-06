// Package scheduler provides schedule registration and trigger calculation (cron/interval/once).
//
// In Phase 2 of the repo refactor, execution is delegated to internal/services/engine.
// The scheduler is responsible only for:
//   - registering schedules
//   - computing next trigger times
//   - enqueueing tasks into the task engine
package scheduler
