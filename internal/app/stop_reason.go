package app

import "pewbot/internal/runtime/lifecycle"

type StopReason = lifecycle.StopReason

const (
	StopUnknown          = lifecycle.StopUnknown
	StopSIGINT           = lifecycle.StopSIGINT
	StopSIGTERM          = lifecycle.StopSIGTERM
	StopFatalError       = lifecycle.StopFatalError
	StopAppStop          = lifecycle.StopAppStop
	StopPluginDisable    = lifecycle.StopPluginDisable
	StopPluginQuarantine = lifecycle.StopPluginQuarantine
	StopConfigReload     = lifecycle.StopConfigReload
)
