package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"pewbot/internal/kit"
)

// ConfigValidator is an optional hook to validate plugin config before applying it.
type ConfigValidator interface {
	ValidateConfig(ctx context.Context, raw json.RawMessage) error
}

// PluginBase is a small helper to make writing plugins faster and safer.
// Typical usage:
//
//	type Plugin struct { core.PluginBase }
//	func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error { p.InitBase(deps, p.Name()); return nil }
//	func (p *Plugin) Start(ctx context.Context) error { p.StartBase(ctx); p.Runner.Go(...); return nil }
//	func (p *Plugin) Stop(ctx context.Context) error { return p.StopBase(ctx) }
type PluginBase struct {
	Log        *slog.Logger
	Deps       PluginDeps
	Runner     *Supervisor
	pluginName string

	ctx context.Context
}

// InitBase wires deps + logger.
func (b *PluginBase) InitBase(deps PluginDeps, pluginName string) {
	b.Deps = deps
	b.pluginName = pluginName
	if deps.Logger != nil {
		b.Log = deps.Logger.With(slog.String("plugin", pluginName))
	} else {
		b.Log = slog.Default().With(slog.String("plugin", pluginName))
	}
}

// StartBase creates a per-plugin supervisor tied to ctx.
func (b *PluginBase) StartBase(ctx context.Context) {
	b.ctx = ctx
	b.Runner = NewSupervisor(ctx, WithLogger(b.Log), WithCancelOnError(false))
}

// StopBase cancels runner + waits bounded by ctx.
func (b *PluginBase) StopBase(ctx context.Context) error {
	if b.Runner == nil {
		return nil
	}
	b.Runner.Cancel()
	err := b.Runner.Wait(ctx)
	b.Runner = nil
	return err
}

// Context returns the plugin runtime context (canceled on stop/disable).
func (b *PluginBase) Context() context.Context { return b.ctx }

// Scheduler helper (namespaced by plugin).
func (b *PluginBase) Every(name string, every time.Duration, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if b.Deps.Services == nil || b.Deps.Services.Scheduler == nil {
		return "", errors.New("scheduler not available")
	}
	return b.Deps.Services.Scheduler.AddInterval(b.ns(name), every, timeout, job)
}

func (b *PluginBase) Cron(name, spec string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if b.Deps.Services == nil || b.Deps.Services.Scheduler == nil {
		return "", errors.New("scheduler not available")
	}
	return b.Deps.Services.Scheduler.AddCron(b.ns(name), spec, timeout, job)
}

func (b *PluginBase) ns(name string) string {
	if b.pluginName == "" {
		return name
	}
	if name == "" {
		return b.pluginName
	}
	return b.pluginName + ":" + name
}

// Notifier helpers.
func (b *PluginBase) Notify(ctx context.Context, n kit.Notification) error {
	if b.Deps.Services == nil || b.Deps.Services.Notifier == nil {
		return errors.New("notifier not available")
	}
	return b.Deps.Services.Notifier.Notify(ctx, n)
}

func (b *PluginBase) Info(chatID int64, text string) error {
	ctx := b.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	n := kit.Notification{
		Channel:  "telegram",
		Priority: 5,
		Target:   kit.ChatTarget{ChatID: chatID},
		Text:     text,
	}
	return b.Notify(cctx, n)
}

// DecodePluginConfig decodes per-plugin raw json into a typed config struct.
func DecodePluginConfig[T any](raw json.RawMessage) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}
