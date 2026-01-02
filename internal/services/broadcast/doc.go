// Package broadcast provides a fan-out message broadcaster.
//
// The broadcaster is used when the same text needs to be sent to many chat
// targets with bounded concurrency, rate limiting, and retry.
//
// Concepts
//
// A broadcast is represented as a Job. Jobs are created with NewJob and then
// started with StartJob. The service maintains a worker pool that processes the
// per-target sends.
//
// Delivery semantics
//
// The broadcaster is best-effort. It retries failed sends up to a configured
// maximum and records per-target results in its in-memory job state. The service
// is designed to be safe to start/stop at runtime; jobs submitted while stopped
// are kept pending and will run when the service starts.
//
// Naming
//
// Job names are intended for observability and should be namespaced by the
// caller (for example "speedtest:weekly-report") to avoid collisions.
package broadcast
