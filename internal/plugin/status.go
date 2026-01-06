package plugin

import ops "pewbot/internal/plugin/ops"

// Re-export operational status types from plugin/ops so callers can continue to
// refer to them via plugin.* while avoiding import cycles with the router.

type PluginsSnapshot = ops.PluginsSnapshot

type PluginStatus = ops.PluginStatus

type PluginHealthResult = ops.PluginHealthResult
