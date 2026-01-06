// Package notify provides a lightweight notification service.
//
// Notifications are small, high-signal messages intended for operators (for
// example alerts, status updates, or action confirmations). A notification
// contains a priority, a target chat (optionally with a thread/topic), and
// send options such as "disable link preview".
//
// # Transport
//
// The service delegates delivery to a kit.Adapter implementation (e.g. the
// Telegram adapter). This keeps notification formatting and throttling policies
// centralized in the adapter while allowing plugins to emit notifications
// without depending on a specific messaging platform.
//
// # History
//
// For debugging and operator visibility, the service keeps a small in-memory
// history of recently emitted notifications.
package notifier
