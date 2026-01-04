package storage

// Package storage provides a minimal persistence layer used by the bot.
//
// It currently supports:
//   - Audit log appends (operator actions)
//   - Optional notifier dedup state (to survive restarts)
