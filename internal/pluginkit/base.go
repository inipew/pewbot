package pluginkit

import (
	"context"

	"pewbot/internal/core"
)

// EnhancedPluginBase extends core.PluginBase with plugin-scoped helper APIs.
//
// This is an opt-in layer: existing plugins can keep using core.PluginBase.
// New plugins can embed EnhancedPluginBase to reduce boilerplate and avoid
// schedule name collisions (via automatic namespacing).
type EnhancedPluginBase struct {
	// Embed original base for backwards compatibility.
	core.PluginBase

	// New helper APIs.
	schedule *ScheduleHelper
	notify   *NotifyHelper
}

// InitEnhanced initializes the embedded core.PluginBase and constructs helpers.
func (b *EnhancedPluginBase) InitEnhanced(deps core.PluginDeps, pluginName string) {
	b.InitBase(deps, pluginName)
	// Helpers are nil-safe; they may wrap nil services in minimal environments.
	b.schedule = NewScheduleHelper(pluginName, deps.Services)
	b.notify = NewNotifyHelper(pluginName, deps)
}

// StartEnhanced extends StartBase with helper context binding.
func (b *EnhancedPluginBase) StartEnhanced(ctx context.Context) {
	b.StartBase(ctx)
	if b.schedule != nil {
		b.schedule.bindContext(ctx)
	}
	if b.notify != nil {
		b.notify.bindContext(ctx)
	}
}

// StopEnhanced extends StopBase with helper auto cleanup.
func (b *EnhancedPluginBase) StopEnhanced(ctx context.Context) error {
	if b.schedule != nil {
		b.schedule.cleanup()
	}
	return b.StopBase(ctx)
}

// Schedule returns the plugin-scoped scheduler helper.
func (b *EnhancedPluginBase) Schedule() *ScheduleHelper { return b.schedule }

// Notify returns the notifier helper.
func (b *EnhancedPluginBase) Notify() *NotifyHelper { return b.notify }
