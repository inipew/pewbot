package core

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"pewbot/internal/adapters/telegram"
	"pewbot/internal/kit"
	"pewbot/internal/services/broadcast"
	"pewbot/internal/services/logging"
	"pewbot/internal/services/notify"
	"pewbot/internal/services/scheduler"
)

type App struct {
	cfgPath string

	cfgm *ConfigManager
	sup  *Supervisor

	log  *slog.Logger
	logs *logging.Service

	adapter kit.Adapter

	sched *scheduler.Service
	bcast *broadcast.Service
	notif *notify.Service

	cmdm *CommandManager
	pm   *PluginManager

	updates chan kit.Update
}

func NewApp(cfgPath string) (*App, error) {
	cfgm := NewConfigManager(cfgPath)
	cfg, err := cfgm.Load()
	if err != nil {
		return nil, err
	}

	// Adapter config mapping
	bootLog := slog.New(logging.NewPrettyHandler(logging.Stdout(), slog.LevelInfo)).With(slog.String("comp", "telegram"))

	// Adapter config mapping
	ad, err := telegram.New(telegram.Config{
		Token:          cfg.Telegram.Token,
		PollTimeoutSec: cfg.Telegram.PollTimeoutSec,
	}, bootLog)
	if err != nil {
		return nil, err
	}

	// Logging service mapping
	logSvc, log := logging.New(logging.Config{
		Level:   cfg.Logging.Level,
		Console: cfg.Logging.Console,
		File: logging.FileConfig{
			Enabled: cfg.Logging.File.Enabled,
			Path:    cfg.Logging.File.Path,
		},
		Telegram: logging.TelegramConfig{
			Enabled:    cfg.Logging.Telegram.Enabled,
			ThreadID:   cfg.Logging.Telegram.ThreadID,
			MinLevel:   cfg.Logging.Telegram.MinLevel,
			RatePerSec: cfg.Logging.Telegram.RatePerSec,
		},
	}, ad)
	log = log.With(slog.String("comp", "app"))

	// Set Telegram log target (chat + thread)
	if strings.TrimSpace(cfg.Telegram.GroupLog) != "" {
		if chatID, err := strconv.ParseInt(strings.TrimSpace(cfg.Telegram.GroupLog), 10, 64); err == nil {
			logSvc.SetTelegramTarget(chatID, cfg.Logging.Telegram.ThreadID)
		}
	}

	// Services mapping
	schedSvc := scheduler.New(scheduler.Config{
		Enabled:          cfg.Scheduler.Enabled,
		Workers:          cfg.Scheduler.Workers,
		DefaultTimeoutMS: cfg.Scheduler.DefaultTimeoutMS,
		HistorySize:      cfg.Scheduler.HistorySize,
		Timezone:         cfg.Scheduler.Timezone,
		RetryMax:         cfg.Scheduler.RetryMax,
	}, log.With(slog.String("comp", "scheduler")))

	bcastSvc := broadcast.New(broadcast.Config{
		Enabled:    cfg.Broadcaster.Enabled,
		Workers:    cfg.Broadcaster.Workers,
		RatePerSec: cfg.Broadcaster.RatePerSec,
		RetryMax:   cfg.Broadcaster.RetryMax,
	}, ad, log)

	notifSvc := notify.New(ad, log.With(slog.String("comp", "notifier")))

	serv := &Services{
		Scheduler:   schedSvc,
		Broadcaster: bcastSvc,
		Notifier:    notifSvc,
	}

	cmdm := NewCommandManager(log.With(slog.String("comp", "commands")),
		ad, cfgm, serv, cfg.Telegram.OwnerUserIDs)

	pm := NewPluginManager(log.With(slog.String("comp", "plugins")),
		cfgm, PluginDeps{
			Logger:      log,
			Adapter:     ad,
			Config:      cfgm,
			Services:    serv,
			OwnerUserID: cfg.Telegram.OwnerUserIDs,
		}, cmdm)

	return &App{
		cfgPath: cfgPath,
		cfgm:    cfgm,
		log:     log,
		logs:    logSvc,
		adapter: ad,
		sched:   schedSvc,
		bcast:   bcastSvc,
		notif:   notifSvc,
		cmdm:    cmdm,
		pm:      pm,
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
	// transactional config reload: validate before commit/publish
	if a.cfgm != nil {
		a.cfgm.SetLogger(a.log.With(slog.String("comp", "config")))
		a.cfgm.SetValidator(func(c context.Context, cfg *Config) error {
			// global validation
			if cfg.Scheduler.Workers < 0 {
				return fmt.Errorf("scheduler.workers must be >= 0")
			}
			if cfg.Scheduler.RetryMax < 0 {
				return fmt.Errorf("scheduler.retry_max must be >= 0")
			}
			if cfg.Broadcaster.Workers < 0 {
				return fmt.Errorf("broadcaster.workers must be >= 0")
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

	if a.sched.Enabled() {
		a.sched.Start(a.sup.Context())
	}
	if a.bcast.Enabled() {
		a.bcast.Start(a.sup.Context())
	}

	if err := a.pm.StartAll(a.sup.Context()); err != nil {
		return err
	}

	a.sup.Go("commands.dispatch", func(c context.Context) error {
		return a.cmdm.DispatchLoop(c, a.updates)
	})

	// hot reload config fan-out
	sub := a.cfgm.Subscribe(8)
	a.sup.Go0("config.reload", func(c context.Context) {
		// Track last applied config to generate a safe diff summary for logging.
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
					a.log.Debug("config change summary", append([]any{slog.String("changed", strings.Join(sections, ","))}, attrsToAny(attrs)...)...)
					if len(pluginChanged) > 0 {
						a.log.Debug("plugin config changes detected", slog.Any("plugins", pluginChanged))
					}
				} else {
					a.log.Debug("config reload received, but no effective changes detected")
				}
				lastApplied = newCfg

				// apply logging updates
				a.logs.Apply(logging.Config{
					Level:   newCfg.Logging.Level,
					Console: newCfg.Logging.Console,
					File: logging.FileConfig{
						Enabled: newCfg.Logging.File.Enabled,
						Path:    newCfg.Logging.File.Path,
					},
					Telegram: logging.TelegramConfig{
						Enabled:    newCfg.Logging.Telegram.Enabled,
						ThreadID:   newCfg.Logging.Telegram.ThreadID,
						MinLevel:   newCfg.Logging.Telegram.MinLevel,
						RatePerSec: newCfg.Logging.Telegram.RatePerSec,
					},
				})

				// update log target
				if strings.TrimSpace(newCfg.Telegram.GroupLog) != "" {
					if chatID, err := strconv.ParseInt(strings.TrimSpace(newCfg.Telegram.GroupLog), 10, 64); err == nil {
						a.logs.SetTelegramTarget(chatID, newCfg.Logging.Telegram.ThreadID)
					}
				} else {
					// allow clearing target via config hot-reload
					a.logs.SetTelegramTarget(0, 0)
				}

				// Update owner list used for AccessOwnerOnly checks and plugin deps.
				a.cmdm.SetOwners(newCfg.Telegram.OwnerUserIDs)
				a.pm.SetOwnerUserIDs(newCfg.Telegram.OwnerUserIDs)

				// apply scheduler/broadcast updates (live)
				prevSchedEnabled := a.sched.Enabled()
				prevBcastEnabled := a.bcast.Enabled()
				a.sched.Apply(scheduler.Config{
					Enabled:          newCfg.Scheduler.Enabled,
					Workers:          newCfg.Scheduler.Workers,
					DefaultTimeoutMS: newCfg.Scheduler.DefaultTimeoutMS,
					HistorySize:      newCfg.Scheduler.HistorySize,
					Timezone:         newCfg.Scheduler.Timezone,
					RetryMax:         newCfg.Scheduler.RetryMax,
				})
				a.bcast.Apply(broadcast.Config{
					Enabled:    newCfg.Broadcaster.Enabled,
					Workers:    newCfg.Broadcaster.Workers,
					RatePerSec: newCfg.Broadcaster.RatePerSec,
					RetryMax:   newCfg.Broadcaster.RetryMax,
				})

				// enable/disable services on the fly (was previously not handled)
				if prevSchedEnabled && !newCfg.Scheduler.Enabled {
					a.log.Info("scheduler disabled via config")
					stopCtx, cancel := context.WithTimeout(c, 3*time.Second)
					a.sched.Stop(stopCtx)
					cancel()
				} else if !prevSchedEnabled && newCfg.Scheduler.Enabled {
					a.log.Info("scheduler enabled via config")
					a.sched.Start(c)
				}

				if prevBcastEnabled && !newCfg.Broadcaster.Enabled {
					a.log.Info("broadcaster disabled via config")
					stopCtx, cancel := context.WithTimeout(c, 3*time.Second)
					a.bcast.Stop(stopCtx)
					cancel()
				} else if !prevBcastEnabled && newCfg.Broadcaster.Enabled {
					a.log.Info("broadcaster enabled via config")
					a.bcast.Start(c)
				}

				// apply plugin enable/disable + per-plugin config
				a.pm.OnConfigUpdate(c, newCfg)

				// Keep the final log line concise and human-friendly (details are in debug logs).
				if len(sections) > 0 {
					a.log.Info("config reloaded", append([]any{slog.String("changed", strings.Join(sections, ","))}, attrsToAny(attrs)...)...)
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
	a.log.Info("stopping", slog.String("reason", string(reason)))

	// First, cancel the app run context so background loops start unwinding immediately.
	a.sup.Cancel()

	// Helper: run a shutdown step with an upper bound so one component can't stall the whole stop.
	step := func(name string, max time.Duration, fn func(context.Context) error) {
		start := time.Now()
		a.log.Debug("stop step begin", slog.String("name", name), slog.Duration("max", max))

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
		go func() { done <- fn(stepCtx) }()

		select {
		case <-ctx.Done():
			a.log.Warn("stop step cancelled", slog.String("name", name), slog.String("err", ctx.Err().Error()))
		case err := <-done:
			if err != nil {
				a.log.Warn("stop step error", slog.String("name", name), slog.String("err", err.Error()))
			}
			took := time.Since(start)
			if took >= 500*time.Millisecond {
				a.log.Info("stop step end", slog.String("name", name), slog.Duration("took", took))
			} else {
				a.log.Debug("stop step end", slog.String("name", name), slog.Duration("took", took))
			}
		case <-stepCtx.Done():
			// step budget exceeded
			a.log.Warn("stop step timeout (continuing)", slog.String("name", name), slog.String("err", stepCtx.Err().Error()))
		}
	}

	// Stop plugins first (they may depend on services). StopAll is timeout-safe per-plugin.
	step("plugins", 4*time.Second, func(c context.Context) error { a.pm.StopAll(c, reason); return nil })

	// Stop services (order: scheduler/broadcaster/notifier/adapter)
	step("scheduler", 2*time.Second, func(c context.Context) error { a.sched.Stop(c); return nil })
	step("broadcaster", 2*time.Second, func(c context.Context) error { a.bcast.Stop(c); return nil })
	step("notifier", 1*time.Second, func(c context.Context) error { a.notif.Stop(c); return nil })
	step("adapter", 2*time.Second, func(c context.Context) error { return a.adapter.Stop(c) })

	// Finally, wait for supervised goroutines (config watch/reload, command dispatcher, etc.)
	step("supervisor", 2*time.Second, func(c context.Context) error { return a.sup.Wait(c) })

	a.log.Info("stopped")
	return nil
}
