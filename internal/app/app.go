package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"pewbot/internal/config"
	"pewbot/internal/eventbus"
	"pewbot/internal/notifier"
	"pewbot/internal/observability/pprof"
	"pewbot/internal/storage"
	"pewbot/internal/task/engine"
	"pewbot/internal/task/scheduler"
	kit "pewbot/internal/transport"
	telegram "pewbot/internal/transport/telegram/adapter"
	logx "pewbot/pkg/logx"
)

type App struct {
	cfgPath string

	cfgm *ConfigManager
	sup  *Supervisor

	log   logx.Logger
	logs  *logx.Service
	bus   eventbus.Bus
	store storage.Store

	adapter kit.Adapter

	engine *engine.Service
	sched  *scheduler.Service
	notif  *notifier.Service
	pprof  *pprof.Service

	cmdm *CommandManager
	pm   *PluginManager

	serv *Services

	updates chan kit.Update
}

func NewApp(cfgPath string) (*App, error) {
	cfgm := NewConfigManager(cfgPath)
	cfg, err := cfgm.Load()
	if err != nil {
		return nil, err
	}

	// Adapter config mapping
	bootLog := logx.NewConsole("INFO").With(logx.String("comp", "telegram"))

	pollTimeout, err := parseDurationOrDefault("telegram.poll_timeout", cfg.Telegram.PollTimeout, 10*time.Second)
	if err != nil {
		return nil, err
	}
	ad, err := telegram.New(telegram.Config{
		Token:       cfg.Telegram.Token,
		PollTimeout: pollTimeout,
	}, bootLog)
	if err != nil {
		return nil, err
	}

	// Logging service mapping
	// Important: logx.New() calls Apply() immediately. If Telegram logging is enabled but the target
	// chat/thread isn't configured yet, Apply() will emit a warning. To avoid a false-positive warning,
	// we bootstrap with Telegram logging disabled, set the target, then Apply() the final config.
	baseLogCfg := logx.Config{
		Level:   cfg.Logging.Level,
		Console: cfg.Logging.Console,
		File: logx.FileConfig{
			Enabled: cfg.Logging.File.Enabled,
			Path:    cfg.Logging.File.Path,
		},
		Telegram: logx.TelegramConfig{
			Enabled:    false, // set target first, then enable via Apply()
			ThreadID:   cfg.Logging.Telegram.ThreadID,
			MinLevel:   cfg.Logging.Telegram.MinLevel,
			RatePerSec: cfg.Logging.Telegram.RatePerSec,
		},
	}
	logSvc, log := logx.New(baseLogCfg, ad)
	log = log.With(logx.String("comp", "app"))

	// Set Telegram log target (chat + thread)
	if strings.TrimSpace(cfg.Telegram.GroupLog) != "" {
		if chatID, err := strconv.ParseInt(strings.TrimSpace(cfg.Telegram.GroupLog), 10, 64); err == nil {
			logSvc.SetTelegramTarget(chatID, cfg.Logging.Telegram.ThreadID)
		}
	}

	// Apply final logging config (including Telegram enable flag).
	finalLogCfg := baseLogCfg
	finalLogCfg.Telegram.Enabled = cfg.Logging.Telegram.Enabled
	logSvc.Apply(finalLogCfg)

	bus := eventbus.New()

	// Storage (optional)
	var store storage.Store
	if sc, enabled, err := mapStorageConfig(cfg); err != nil {
		return nil, err
	} else if enabled {
		st, err := storage.Open(sc, log.With(logx.String("comp", "storage")))
		if err != nil {
			return nil, err
		}
		store = st
		log.Info("storage enabled", logx.String("driver", sc.Driver))
	}

	// Services mapping
	engCfg, err := mapTaskEngineConfig(cfg)
	if err != nil {
		return nil, err
	}

	engineSvc := engine.New(engCfg, log.With(logx.String("comp", "taskengine")), bus)

	schedSvc := scheduler.New(scheduler.Config{
		Enabled:        cfg.Scheduler.Enabled,
		Workers:        engCfg.Workers,
		DefaultTimeout: engCfg.DefaultTimeout,
		HistorySize:    engCfg.HistorySize,
		Timezone:       cfg.Scheduler.Timezone,
		RetryMax:       engCfg.RetryMax,
	}, engineSvc, log.With(logx.String("comp", "scheduler")), bus)

	ncfg, err := mapNotifierConfig(cfg)
	if err != nil {
		return nil, err
	}
	notifSvc := notifier.New(ncfg, ad, log.With(logx.String("comp", "notifier")), bus, store)

	// pprof service mapping (optional)
	pprofCfg, err := mapPprofConfig(cfg)
	if err != nil {
		return nil, err
	}
	pprofSvc := pprof.New(pprofCfg, log.With(logx.String("comp", "pprof")))

	serv := &Services{
		Scheduler:          schedSvc,
		Notifier:           notifSvc,
		RuntimeSupervisors: NewSupervisorRegistry(),
	}

	cmdm := NewCommandManager(log.With(logx.String("comp", "commands")),
		ad, cfgm, serv, cfg.Telegram.OwnerUserIDs)

	pm := NewPluginManager(log.With(logx.String("comp", "plugins")),
		cfgm, PluginDeps{
			Logger:      log,
			Adapter:     ad,
			Config:      cfgm,
			Services:    serv,
			Bus:         bus,
			Store:       store,
			OwnerUserID: cfg.Telegram.OwnerUserIDs,
		}, cmdm)
	// Expose plugin runtime state for operational commands.
	serv.Plugins = pm

	return &App{
		cfgPath: cfgPath,
		cfgm:    cfgm,
		log:     log,
		logs:    logSvc,
		bus:     bus,
		store:   store,
		adapter: ad,
		engine:  engineSvc,
		sched:   schedSvc,
		notif:   notifSvc,
		pprof:   pprofSvc,
		cmdm:    cmdm,
		pm:      pm,
		serv:    serv,
		updates: make(chan kit.Update, 256),
	}, nil
}

func (a *App) Plugins() *PluginManager { return a.pm }

// Done is closed when the app supervisor context is canceled (fatal error or Stop()).
func (a *App) Done() <-chan struct{} {
	if a.sup == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return a.sup.Context().Done()
}

// Err returns the first fatal error observed by the supervisor (if any).
func (a *App) Err() error {
	if a.sup == nil {
		return nil
	}
	return a.sup.Err()
}

func (a *App) Start(ctx context.Context) error {
	a.sup = NewSupervisor(ctx, WithLogger(a.log), WithCancelOnError(true))
	if a.serv != nil {
		a.serv.AppSupervisor = a.sup
		if a.serv.RuntimeSupervisors == nil {
			a.serv.RuntimeSupervisors = NewSupervisorRegistry()
		}
	}
	// transactional config reload: validate before commit/publish
	if a.cfgm != nil {
		a.cfgm.SetLogger(a.log.With(logx.String("comp", "config")))
		a.cfgm.SetValidator(func(c context.Context, cfg *Config) error {
			// global validation
			// Legacy execution fields under scheduler (still accepted, but deprecated)
			if cfg.Scheduler.Workers < 0 {
				return fmt.Errorf("scheduler.workers must be >= 0")
			}
			if cfg.Scheduler.HistorySize < 0 {
				return fmt.Errorf("scheduler.history_size must be >= 0")
			}
			if cfg.Scheduler.RetryMax < 0 {
				return fmt.Errorf("scheduler.retry_max must be >= 0")
			}
			if _, err := parseDurationField("scheduler.default_timeout", cfg.Scheduler.DefaultTimeout); err != nil {
				return err
			}

			// New engine config
			if cfg.TaskEngine != nil {
				if cfg.TaskEngine.Workers < 0 {
					return fmt.Errorf("task_engine.workers must be >= 0")
				}
				if cfg.TaskEngine.QueueSize < 0 {
					return fmt.Errorf("task_engine.queue_size must be >= 0")
				}
				if cfg.TaskEngine.HistorySize < 0 {
					return fmt.Errorf("task_engine.history_size must be >= 0")
				}
				if cfg.TaskEngine.RetryMax < 0 {
					return fmt.Errorf("task_engine.retry_max must be >= 0")
				}
				if _, err := parseDurationField("task_engine.default_timeout", cfg.TaskEngine.DefaultTimeout); err != nil {
					return err
				}
				if _, err := parseDurationField("task_engine.max_queue_delay", cfg.TaskEngine.MaxQueueDelay); err != nil {
					return err
				}
				if cfg.Scheduler.Enabled && cfg.TaskEngine.Enabled != nil && !*cfg.TaskEngine.Enabled {
					return fmt.Errorf("task_engine.enabled cannot be false while scheduler.enabled is true")
				}
			}

			// duration/timezone validation (reject bad hot-reload)
			if _, err := parseDurationField("telegram.poll_timeout", cfg.Telegram.PollTimeout); err != nil {
				return err
			}
			if tz := strings.TrimSpace(cfg.Scheduler.Timezone); tz != "" {
				if _, err := time.LoadLocation(tz); err != nil {
					return fmt.Errorf("scheduler.timezone: invalid %q: %w", tz, err)
				}
			}
			// pprof validation (safe even when disabled)
			if _, err := mapPprofConfig(cfg); err != nil {
				return err
			}
			// notifier validation (parse durations + basic bounds)
			if _, err := mapNotifierConfig(cfg); err != nil {
				return err
			}
			// storage validation
			if _, _, err := mapStorageConfig(cfg); err != nil {
				return err
			}
			// per-plugin validation
			if a.pm != nil {
				return a.pm.ValidateConfig(c, cfg)
			}
			return nil
		})
	}

	if err := a.adapter.Start(a.sup.Context(), a.updates); err != nil {
		return err
	}
	// Expose adapter supervisor for operational visibility.
	if a.serv != nil {
		if sp, ok := a.adapter.(interface{ Supervisor() *Supervisor }); ok {
			if sup := sp.Supervisor(); sup != nil {
				a.serv.RuntimeSupervisors.Set("telegram.adapter", sup)
			}
		}
	}

	if a.notif != nil && a.notif.Enabled() {
		a.notif.Start(a.sup.Context())
		if a.serv != nil {
			if sp, ok := any(a.notif).(interface{ Supervisor() *Supervisor }); ok {
				if sup := sp.Supervisor(); sup != nil {
					a.serv.RuntimeSupervisors.Set("notifier", sup)
				}
			}
		}
	}
	if a.engine != nil && a.engine.Enabled() {
		a.engine.Start(a.sup.Context())
		if a.serv != nil {
			if sp, ok := any(a.engine).(interface{ Supervisor() *Supervisor }); ok {
				if sup := sp.Supervisor(); sup != nil {
					a.serv.RuntimeSupervisors.Set("task.engine", sup)
				}
			}
		}
	}
	if a.sched.Enabled() {
		a.sched.Start(a.sup.Context())
	}
	if a.pprof != nil && a.pprof.Enabled() {
		a.pprof.Start(a.sup.Context())
		if a.serv != nil {
			if sp, ok := any(a.pprof).(interface{ Supervisor() *Supervisor }); ok {
				if sup := sp.Supervisor(); sup != nil {
					a.serv.RuntimeSupervisors.Set("pprof", sup)
				}
			}
		}
	}

	if err := a.pm.StartAll(a.sup.Context()); err != nil {
		return err
	}

	a.sup.Go("commands.dispatch", func(c context.Context) error {
		return a.cmdm.DispatchLoop(c, a.updates)
	})

	// Optional: log events for observability/debug (components can also subscribe themselves).
	if a.bus != nil {
		events, unsub := a.bus.Subscribe(128)
		a.sup.Go0("eventbus.log", func(c context.Context) {
			defer unsub()
			for {
				select {
				case <-c.Done():
					return
				case e, ok := <-events:
					if !ok {
						return
					}
					// Keep this debug-level to avoid noise for frequent schedulers.
					a.log.Debug("event", logx.String("type", e.Type), logx.Time("time", e.Time))
				}
			}
		})
	}

	// hot reload config fan-out
	sub := a.cfgm.Subscribe(8)
	a.sup.Go0("config.reload", func(c context.Context) {
		defer a.cfgm.Unsubscribe(sub)
		// Track last applied config to generate a safe diff summary for logx.
		lastApplied := a.cfgm.Get()
		for {
			select {
			case <-c.Done():
				return
			case newCfg, ok := <-sub:
				if !ok {
					return
				}
				// Coalesce bursts: keep only the latest config in the channel.
				for {
					select {
					case newer := <-sub:
						if newer != nil {
							newCfg = newer
						}
					default:
						goto APPLY
					}
				}
			APPLY:
				sections, attrs, pluginChanged := SummarizeConfigChange(lastApplied, newCfg)
				if len(sections) > 0 {
					fields := append([]logx.Field{logx.String("changed", strings.Join(sections, ","))}, attrs...)
					a.log.Debug("config change summary", fields...)
					if len(pluginChanged) > 0 {
						a.log.Debug("plugin config changes detected", logx.Any("plugins", pluginChanged))
					}
				} else {
					a.log.Debug("config reload received, but no effective changes detected")
				}
				lastApplied = newCfg

				for _, s := range sections {
					if s == "storage" {
						a.log.Warn("storage config changed; restart required for changes to take effect")
						break
					}
				}

				// update log target first (so Apply() doesn't warn when Telegram logging is enabled)
				if strings.TrimSpace(newCfg.Telegram.GroupLog) != "" {
					if chatID, err := strconv.ParseInt(strings.TrimSpace(newCfg.Telegram.GroupLog), 10, 64); err == nil {
						a.logs.SetTelegramTarget(chatID, newCfg.Logging.Telegram.ThreadID)
					}
				} else {
					// allow clearing target via config hot-reload
					a.logs.SetTelegramTarget(0, 0)
				}

				// apply logging updates
				a.logs.Apply(logx.Config{
					Level:   newCfg.Logging.Level,
					Console: newCfg.Logging.Console,
					File: logx.FileConfig{
						Enabled: newCfg.Logging.File.Enabled,
						Path:    newCfg.Logging.File.Path,
					},
					Telegram: logx.TelegramConfig{
						Enabled:    newCfg.Logging.Telegram.Enabled,
						ThreadID:   newCfg.Logging.Telegram.ThreadID,
						MinLevel:   newCfg.Logging.Telegram.MinLevel,
						RatePerSec: newCfg.Logging.Telegram.RatePerSec,
					},
				})

				// Update owner list used for AccessOwnerOnly checks and plugin deps.
				a.cmdm.SetOwners(newCfg.Telegram.OwnerUserIDs)
				a.pm.SetOwnerUserIDs(newCfg.Telegram.OwnerUserIDs)

				// apply scheduler/taskengine updates (live)
				prevSchedEnabled := a.sched.Enabled()
				prevEngEnabled := false
				if a.engine != nil {
					prevEngEnabled = a.engine.Enabled()
				}

				newEngCfg, err := mapTaskEngineConfig(newCfg)
				if err != nil {
					a.log.Warn("invalid task_engine config; keeping previous", logx.Any("err", err))
				} else if a.engine != nil {
					a.engine.Apply(c, newEngCfg)
				}

				// Scheduler is trigger-only; keep its config minimal, but pass effective execution settings for snapshots.
				appliedEng := newEngCfg
				if err != nil && a.engine != nil {
					es := a.engine.Snapshot()
					appliedEng = engine.Config{
						Enabled:        es.Enabled,
						Workers:        es.Workers,
						QueueSize:      es.QueueCap,
						DefaultTimeout: es.DefaultTimeout,
						MaxQueueDelay:  es.MaxQueueDelay,
						HistorySize:    len(es.History),
						RetryMax:       es.RetryMax,
					}
				}
				a.sched.Apply(scheduler.Config{
					Enabled:        newCfg.Scheduler.Enabled,
					Workers:        appliedEng.Workers,
					DefaultTimeout: appliedEng.DefaultTimeout,
					HistorySize:    appliedEng.HistorySize,
					Timezone:       newCfg.Scheduler.Timezone,
					RetryMax:       appliedEng.RetryMax,
				})

				newSchedEnabled := newCfg.Scheduler.Enabled
				newEngEnabled := appliedEng.Enabled

				// enable/disable services on the fly (scheduler first on shutdown; engine first on startup)
				if prevSchedEnabled && !newSchedEnabled {
					a.log.Info("scheduler disabled via config")
					stopCtx, cancel := context.WithTimeout(c, 3*time.Second)
					a.sched.Stop(stopCtx)
					cancel()
				}
				if prevEngEnabled && !newEngEnabled {
					a.log.Info("task engine disabled via config")
					stopCtx, cancel := context.WithTimeout(c, 3*time.Second)
					if a.engine != nil {
						a.engine.Stop(stopCtx)
					}
					cancel()
				}
				if !prevEngEnabled && newEngEnabled {
					a.log.Info("task engine enabled via config")
					if a.engine != nil {
						a.engine.Start(c)
					}
				}
				if !prevSchedEnabled && newSchedEnabled {
					a.log.Info("scheduler enabled via config")
					a.sched.Start(c)
				}

				// apply notifier updates (live)
				if a.notif != nil {
					prevNotifEnabled := a.notif.Enabled()
					ncfg, err := mapNotifierConfig(newCfg)
					if err != nil {
						a.log.Warn("invalid notifier config; keeping previous", logx.Any("err", err))
					} else {
						a.notif.Apply(ncfg)
						if prevNotifEnabled && !ncfg.Enabled {
							a.log.Info("notifier disabled via config")
							stopCtx, cancel := context.WithTimeout(c, 3*time.Second)
							a.notif.Stop(stopCtx)
							cancel()
						} else if !prevNotifEnabled && ncfg.Enabled {
							a.log.Info("notifier enabled via config")
							a.notif.Start(c)
						}
					}
				}

				// apply pprof updates (live)
				if a.pprof != nil {
					ppc, err := mapPprofConfig(newCfg)
					if err != nil {
						a.log.Warn("invalid pprof config; keeping previous", logx.Any("err", err))
					} else {
						a.pprof.Reconfigure(c, ppc)
					}
				}

				// apply plugin enable/disable + per-plugin config
				a.pm.OnConfigUpdate(c, newCfg)

				// Keep the final log line concise and human-friendly (details are in debug logs).
				if len(sections) > 0 {
					fields := append([]logx.Field{logx.String("changed", strings.Join(sections, ","))}, attrs...)
					a.log.Info("config reloaded", fields...)
				} else {
					a.log.Info("config reloaded (no changes)")
				}
			}
		}
	})

	a.sup.Go("config.watch", func(c context.Context) error {
		return a.cfgm.Watch(c)
	})

	a.log.Info("app started")
	return nil
}

func (a *App) Stop(ctx context.Context, reason StopReason) error {
	if a.sup == nil {
		return nil
	}
	a.log.Info("stopping", logx.String("reason", string(reason)))

	// First, cancel the app run context so background loops start unwinding immediately.
	a.sup.Cancel()

	// Helper: run a shutdown step with an upper bound so one component can't stall the whole stop.
	step := func(name string, max time.Duration, fn func(context.Context) error) {
		start := time.Now()
		a.log.Debug("stop step begin", logx.String("name", name), logx.Duration("max", max))

		stepCtx := ctx
		var cancel context.CancelFunc
		if max > 0 {
			// respect the caller's deadline; never extend it
			if dl, ok := ctx.Deadline(); ok {
				rem := time.Until(dl)
				if rem <= 0 {
					max = 0
				} else if rem < max {
					max = rem
				}
			}
			if max > 0 {
				stepCtx, cancel = context.WithTimeout(ctx, max)
				defer cancel()
			}
		}

		done := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					done <- fmt.Errorf("panic in stop step %s: %v", name, r)
				}
			}()
			done <- fn(stepCtx)
		}()

		select {
		case err := <-done:
			if err != nil {
				a.log.Warn("stop step error", logx.String("name", name), logx.String("err", err.Error()))
			}
			took := time.Since(start)
			if took >= 500*time.Millisecond {
				a.log.Info("stop step end", logx.String("name", name), logx.Duration("took", took))
			} else {
				a.log.Debug("stop step end", logx.String("name", name), logx.Duration("took", took))
			}
		case <-stepCtx.Done():
			// Contract: fn MUST honor stepCtx and return promptly. If it doesn't, log a leak signal.
			elapsed := time.Since(start)
			a.log.Warn(
				"stop step deadline reached (continuing)",
				logx.String("name", name),
				logx.String("err", stepCtx.Err().Error()),
				logx.Duration("elapsed", elapsed),
			)
			// Leak logging: observe when/if the step eventually finishes.
			go func() {
				err := <-done
				took := time.Since(start)
				if err != nil {
					a.log.Warn("stop step finished after deadline", logx.String("name", name), logx.String("err", err.Error()), logx.Duration("took", took))
				} else {
					a.log.Info("stop step finished after deadline", logx.String("name", name), logx.Duration("took", took))
				}
			}()
		}
	}

	// Stop plugins first (they may depend on services). StopAll is timeout-safe per-plugin.
	step("plugins", 4*time.Second, func(c context.Context) error { a.pm.StopAll(c, reason); return nil })

	// Stop services (order: scheduler/notifier/adapter)
	step("scheduler", 2*time.Second, func(c context.Context) error { a.sched.Stop(c); return nil })
	step("taskengine", 2*time.Second, func(c context.Context) error {
		if a.engine != nil {
			a.engine.Stop(c)
		}
		return nil
	})
	step("pprof", 1*time.Second, func(c context.Context) error {
		if a.pprof != nil {
			a.pprof.Stop(c)
		}
		return nil
	})
	step("notifier", 1*time.Second, func(c context.Context) error { a.notif.Stop(c); return nil })
	step("adapter", 2*time.Second, func(c context.Context) error { return a.adapter.Stop(c) })
	step("storage", 1*time.Second, func(c context.Context) error {
		if a.store != nil {
			return a.store.Close()
		}
		return nil
	})

	// Finally, wait for supervised goroutines (config watch/reload, command dispatcher, etc.)
	step("supervisor", 2*time.Second, func(c context.Context) error { return a.sup.Wait(c) })

	a.log.Info("stopped")
	if a.logs != nil {
		a.logs.Close()
	}
	return nil
}

func mapTaskEngineConfig(cfg *config.Config) (engine.Config, error) {
	if cfg == nil {
		return engine.Config{}, nil
	}

	// Legacy defaults (applied when config values are omitted or 0)
	enabled := cfg.Scheduler.Enabled
	workers := cfg.Scheduler.Workers
	historySize := cfg.Scheduler.HistorySize
	retryMax := cfg.Scheduler.RetryMax
	queueSize := 256

	defTimeoutStr := cfg.Scheduler.DefaultTimeout
	defTimeoutKey := "scheduler.default_timeout"
	maxQueueDelayStr := ""

	if cfg.TaskEngine != nil {
		if cfg.TaskEngine.Enabled != nil {
			enabled = *cfg.TaskEngine.Enabled
		}
		if cfg.TaskEngine.Workers != 0 {
			workers = cfg.TaskEngine.Workers
		}
		if cfg.TaskEngine.QueueSize != 0 {
			queueSize = cfg.TaskEngine.QueueSize
		}
		if cfg.TaskEngine.HistorySize != 0 {
			historySize = cfg.TaskEngine.HistorySize
		}
		if cfg.TaskEngine.RetryMax != 0 {
			retryMax = cfg.TaskEngine.RetryMax
		}
		if strings.TrimSpace(cfg.TaskEngine.DefaultTimeout) != "" {
			defTimeoutStr = cfg.TaskEngine.DefaultTimeout
			defTimeoutKey = "task_engine.default_timeout"
		}
		if strings.TrimSpace(cfg.TaskEngine.MaxQueueDelay) != "" {
			maxQueueDelayStr = cfg.TaskEngine.MaxQueueDelay
		}

		// Safety: avoid a config where scheduler triggers run but engine is explicitly disabled.
		if cfg.Scheduler.Enabled && cfg.TaskEngine.Enabled != nil && !*cfg.TaskEngine.Enabled {
			return engine.Config{}, fmt.Errorf("task_engine.enabled cannot be false while scheduler.enabled is true")
		}
	}

	if workers <= 0 {
		workers = 2
	}
	if queueSize <= 0 {
		queueSize = 256
	}
	if historySize < 0 {
		historySize = 0
	} else if historySize == 0 {
		historySize = 200
	}
	if retryMax < 0 {
		retryMax = 0
	} else if retryMax == 0 {
		retryMax = 3
	}

	defTimeout, err := parseDurationField(defTimeoutKey, defTimeoutStr)
	if err != nil {
		return engine.Config{}, err
	}
	maxQueueDelay, err := parseDurationField("task_engine.max_queue_delay", maxQueueDelayStr)
	if err != nil {
		return engine.Config{}, err
	}

	return engine.Config{
		Enabled:        enabled,
		Workers:        workers,
		QueueSize:      queueSize,
		DefaultTimeout: defTimeout,
		MaxQueueDelay:  maxQueueDelay,
		HistorySize:    historySize,
		RetryMax:       retryMax,
	}, nil
}
