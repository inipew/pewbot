package ops

import "time"

// PluginsSnapshot is a point-in-time view of plugin runtime state.
//
// This package intentionally contains *data-only* operational types so both the
// command router and the plugin manager can depend on them without creating
// import cycles.
type PluginsSnapshot struct {
	Time    time.Time      `json:"time"`
	Plugins []PluginStatus `json:"plugins"`
}

// PluginStatus captures enable/run/quarantine and last-known health info.
type PluginStatus struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Running   bool   `json:"running"`
	HasConfig bool   `json:"has_config"`

	Quarantined     bool      `json:"quarantined"`
	QuarantineErr   string    `json:"quarantine_err,omitempty"`
	QuarantineSince time.Time `json:"quarantine_since,omitempty"`

	HasHealthChecker bool `json:"has_health_checker"`
	HealthLoopActive bool `json:"health_loop_active"`

	LastHealth PluginHealthResult `json:"last_health"`
}

// PluginHealthResult is a single health probe outcome.
type PluginHealthResult struct {
	Plugin string    `json:"plugin"`
	At     time.Time `json:"at"`
	Status string    `json:"status,omitempty"`
	Err    string    `json:"err,omitempty"`
	Fails  int       `json:"fails,omitempty"`
}
