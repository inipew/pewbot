// Package logging configures pewbot's structured logging.
//
// The package builds slog handlers based on configuration and can emit logs to
// multiple sinks:
//   - Console (human-friendly pretty output)
//   - File (JSON or text, depending on handler selection)
//   - Telegram (via kit.Adapter) with rate limiting and minimum level
//
// Telegram logging is intended for concise operator visibility. It should be
// configured with an explicit chat target (typically telegram.group_log) and a
// min_level to avoid excessive noise.
package logging
