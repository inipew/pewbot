// Package scheduler provides an in-process task scheduler used by pewbot services
// and plugins.
//
// # Overview
//
// The scheduler runs user-provided jobs with a configurable worker pool and
// optional rate limiting. Jobs are registered under a logical name (e.g.
// "speedtest:auto"). Names are intended to be stable and human readable so that
// tasks can be replaced (upserted) and removed deterministically.
//
// # Schedule formats
//
// The scheduler accepts multiple schedule syntaxes:
//
//   - Cron expressions: 5-field (min hour dom mon dow) or 6-field with optional
//     seconds. Example: "55 * * * *" or "0 */5 * * * *".
//   - Cron descriptors: "@hourly", "@daily", "@every 55m".
//   - Interval durations: Go duration strings like "55m" or "2h30m".
//   - Interval HH:MM: a compact duration format where "00:50" means every 50
//     minutes and "02:30" means every 2 hours 30 minutes.
//
// To force interpretation, callers may prefix the string with "cron:",
// "interval:", or "every:".
//
// # Concurrency and overlap
//
// Jobs run on a worker pool. The TaskOptions overlap policy can be used to
// either allow overlap or skip a run if the previous run is still executing.
// A per-job timeout is applied to each run.
//
// # Lifecycle
//
// The Service can be started/stopped at runtime (e.g. via config hot reload).
// Registering tasks while stopped is supported: definitions are stored and
// applied on the next start.
package scheduler
