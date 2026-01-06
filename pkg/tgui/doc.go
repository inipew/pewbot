// Package tgui provides small Telegram UI helpers:
//   - Inline keyboard builders
//   - Callback data helpers (plugin:action:payload)
//   - A simple, safe message builder with sensible defaults
//
// Design goals:
//   - Ergonomic for plugins (one builder covers text + send options)
//   - Safe by default for Telegram ParseMode="HTML" (auto escaping)
//   - Reusable patterns for many scenarios (status cards, lists, actions)
package tgui
