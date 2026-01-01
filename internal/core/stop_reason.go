package core

// StopReason is used for structured shutdown tracing.
type StopReason string

const (
	StopUnknown       StopReason = "unknown"
	StopSIGINT        StopReason = "sigint"
	StopSIGTERM       StopReason = "sigterm"
	StopFatalError    StopReason = "fatal_error"
	StopAppStop       StopReason = "app_stop"
	StopPluginDisable StopReason = "plugin_disable"
	StopConfigReload  StopReason = "config_reload"
)
