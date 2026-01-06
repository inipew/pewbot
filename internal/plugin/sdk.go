package plugin

import (
	"context"
	"encoding/json"
	"errors"
	logx "pewbot/pkg/logx"
	"time"

	"pewbot/internal/eventbus"
	"pewbot/internal/storage"
	kit "pewbot/internal/transport"
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
	Log        logx.Logger
	Deps       PluginDeps
	Runner     *Supervisor
	pluginName string

	ctx context.Context
}

// Supervisor returns the per-plugin supervisor, if StartBase has been called.
// This lets the core attach additional plugin-scoped goroutines (e.g. health loops)
// so they become owned + joinable under StopBase.
func (b *PluginBase) Supervisor() *Supervisor { return b.Runner }

// Health implements core.HealthChecker for any plugin embedding PluginBase.
//
// It is intentionally lightweight and should never block.
// If a plugin needs richer health reporting, it can override Health().
func (b *PluginBase) Health(ctx context.Context) (string, error) {
	if b == nil {
		return "nil", errors.New("plugin base is nil")
	}
	if b.ctx == nil {
		return "not_started", nil
	}
	select {
	case <-b.ctx.Done():
		return "stopped", b.ctx.Err()
	default:
	}
	return "ok", nil
}

// InitBase wires deps + logger.
func (b *PluginBase) InitBase(deps PluginDeps, pluginName string) {
	b.Deps = deps
	b.pluginName = pluginName
	if !deps.Logger.IsZero() {
		b.Log = deps.Logger.With(logx.String("plugin", pluginName))
	} else {
		b.Log = logx.Nop().With(logx.String("plugin", pluginName))
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

// AppendAudit writes an audit entry to the configured storage (if present).
// Plugins should treat this as best-effort; if storage is disabled, an error is returned.
func (b *PluginBase) AppendAudit(ctx context.Context, e storage.AuditEntry) error {
	if b == nil {
		return errors.New("plugin is nil")
	}
	st := b.Deps.Store
	if st == nil {
		return errors.New("storage not available")
	}
	return st.AppendAudit(ctx, e)
}

// PublishEvent publishes a lightweight event to the in-process event bus (if present).
// This is safe to call from plugins; Publish is non-blocking.
func (b *PluginBase) PublishEvent(typ string, data any) {
	if b == nil {
		return
	}
	bus := b.Deps.Bus
	if bus == nil {
		return
	}
	bus.Publish(eventbus.Event{Type: typ, Data: data})
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
