// Package logx configures pewbot's structured logging.
//
// This repo uses a small wrapper (logx.Logger) on top of zerolog to keep:
//   - Console output readable (short timestamp + short caller)
//   - File output JSON-structured
//   - Optional Telegram sink (min-level + rate limiting)
package logx
