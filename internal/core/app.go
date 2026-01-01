package core

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

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
	ad, err := telegram.New(telegram.Config{
		Token:          cfg.Telegram.Token,
		PollTimeoutSec: cfg.Telegram.PollTimeoutSec,
	}, slog.New(slog.NewTextHandler(logging.Stdout(), &slog.HandlerOptions{Level: slog.LevelInfo})))
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
	}, log)

	bcastSvc := broadcast.New(broadcast.Config{
		Enabled:    cfg.Broadcaster.Enabled,
		Workers:    cfg.Broadcaster.Workers,
		RatePerSec: cfg.Broadcaster.RatePerSec,
		RetryMax:   cfg.Broadcaster.RetryMax,
	}, ad, log)

	notifSvc := notify.New(ad, log)

	serv := &Services{
		Scheduler:   schedSvc,
		Broadcaster: bcastSvc,
		Notifier:    notifSvc,
	}

	cmdm := NewCommandManager(log, ad, cfgm, serv, cfg.Telegram.OwnerUserIDs)

	pm := NewPluginManager(log, cfgm, PluginDeps{
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

func (a *App) Start(ctx context.Context) error {
	a.sup = NewSupervisor(ctx)

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

	a.sup.Go(func(c context.Context) {
		a.cmdm.DispatchLoop(c, a.updates)
	})

	// hot reload config fan-out
	sub := a.cfgm.Subscribe(8)
	a.sup.Go(func(c context.Context) {
		for {
			select {
			case <-c.Done():
				return
			case newCfg := <-sub:
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
				}

				// apply scheduler/broadcast updates (live)
				a.sched.Apply(scheduler.Config{
					Enabled:          newCfg.Scheduler.Enabled,
					Workers:          newCfg.Scheduler.Workers,
					DefaultTimeoutMS: newCfg.Scheduler.DefaultTimeoutMS,
					HistorySize:      newCfg.Scheduler.HistorySize,
					Timezone:         newCfg.Scheduler.Timezone,
				})
				a.bcast.Apply(broadcast.Config{
					Enabled:    newCfg.Broadcaster.Enabled,
					Workers:    newCfg.Broadcaster.Workers,
					RatePerSec: newCfg.Broadcaster.RatePerSec,
					RetryMax:   newCfg.Broadcaster.RetryMax,
				})

				// apply plugin enable/disable + per-plugin config
				a.pm.OnConfigUpdate(c, newCfg)

				a.log.Info("config reloaded")
			}
		}
	})

	a.sup.Go(func(c context.Context) {
		_ = a.cfgm.Watch(c)
	})

	a.log.Info("app started")
	return nil
}

func (a *App) Stop(ctx context.Context) error {
	if a.sup == nil {
		return nil
	}
	a.pm.StopAll(ctx)
	a.sched.Stop(ctx)
	a.bcast.Stop(ctx)
	_ = a.adapter.Stop(ctx)

	a.sup.Stop()
	return nil
}
