package plugin

import (
	"pewbot/internal/config"
	"pewbot/internal/runtime/supervisor"
	"pewbot/internal/transport/telegram/router"
)

// ---- Config ----

type Config = config.Config

type ConfigManager = config.ConfigManager

// PluginConfigRaw is the raw per-plugin config blob inside config.Config.
// It lives in the config package to keep the schema centralized.
type PluginConfigRaw = config.PluginConfigRaw

// ---- Runtime ----

type Supervisor = supervisor.Supervisor

type SupervisorOption = supervisor.SupervisorOption

var NewSupervisor = supervisor.NewSupervisor

var WithLogger = supervisor.WithLogger

var WithCancelOnError = supervisor.WithCancelOnError

// ---- Router API (commands / callbacks) ----

type Access = router.Access

const (
	AccessEveryone  = router.AccessEveryone
	AccessOwnerOnly = router.AccessOwnerOnly
)

type Command = router.Command

type Request = router.Request

type HandlerFunc = router.HandlerFunc

type CallbackHandlerFunc = router.CallbackHandlerFunc

type CallbackRoute = router.CallbackRoute

type CallbackAccess = router.CallbackAccess

const (
	CallbackAccessOwnerOnly = router.CallbackAccessOwnerOnly
	CallbackAccessEveryone  = router.CallbackAccessEveryone
)

type Services = router.Services

type CommandManager = router.CommandManager

// ---- Service ports (scheduler/notifier/plugins) ----

type SchedulerPort = router.SchedulerPort

type NotifierPort = router.NotifierPort

type PluginsPort = router.PluginsPort

// ---- Scheduler option types ----

type TaskOptions = router.TaskOptions

type Snapshot = router.Snapshot

type OverlapPolicy = router.OverlapPolicy

const (
	OverlapAllow         = router.OverlapAllow
	OverlapSkipIfRunning = router.OverlapSkipIfRunning
)
