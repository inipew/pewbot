package pluginkit

import (
	"context"
	"errors"
	"strconv"
	"strings"

	core "pewbot/internal/plugin"
	kit "pewbot/internal/transport"
)

// NotifyHelper is a small ergonomic wrapper around core.NotifierPort.
//
// It provides:
//   - Convenience methods for Info/Warn/Error with default target resolution.
//   - A builder for sending to a custom target.
type NotifyHelper struct {
	pluginName string
	deps       core.PluginDeps
	ctx        context.Context
}

func NewNotifyHelper(pluginName string, deps core.PluginDeps) *NotifyHelper {
	return &NotifyHelper{pluginName: pluginName, deps: deps}
}

func (h *NotifyHelper) bindContext(ctx context.Context) { h.ctx = ctx }

func (h *NotifyHelper) Info(text string) error  { return h.send(5, text) }
func (h *NotifyHelper) Warn(text string) error  { return h.send(7, text) }
func (h *NotifyHelper) Error(text string) error { return h.send(9, text) }

// To returns a builder that sends to a specific target.
func (h *NotifyHelper) To(target kit.ChatTarget) *NotifyBuilder {
	return &NotifyBuilder{helper: h, target: target}
}

func (h *NotifyHelper) send(priority int, text string) error {
	target := h.getDefaultTarget()
	if target.ChatID == 0 {
		return errors.New("no notification target configured")
	}
	return h.sendTo(priority, target, text)
}

func (h *NotifyHelper) sendTo(priority int, target kit.ChatTarget, text string) error {
	if h == nil || h.deps.Services == nil || h.deps.Services.Notifier == nil {
		return errors.New("notifier not available")
	}
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return h.deps.Services.Notifier.Notify(ctx, kit.Notification{
		Priority: priority,
		Target:   target,
		Text:     text,
		Options:  &kit.SendOptions{DisablePreview: true},
	})
}

func (h *NotifyHelper) getDefaultTarget() kit.ChatTarget {
	cfgm := h.deps.Config
	if cfgm == nil {
		return kit.ChatTarget{}
	}
	cfg := cfgm.Get()
	if cfg == nil {
		return kit.ChatTarget{}
	}

	// Prefer group_log if set.
	groupLog := strings.TrimSpace(cfg.Telegram.GroupLog)
	if groupLog != "" {
		if chatID, err := strconv.ParseInt(groupLog, 10, 64); err == nil {
			return kit.ChatTarget{ChatID: chatID, ThreadID: cfg.Logging.Telegram.ThreadID}
		}
	}

	// Fallback to first owner.
	if len(h.deps.OwnerUserID) > 0 {
		return kit.ChatTarget{ChatID: h.deps.OwnerUserID[0]}
	}

	return kit.ChatTarget{}
}

type NotifyBuilder struct {
	helper *NotifyHelper
	target kit.ChatTarget
}

func (b *NotifyBuilder) Info(text string) error  { return b.helper.sendTo(5, b.target, text) }
func (b *NotifyBuilder) Warn(text string) error  { return b.helper.sendTo(7, b.target, text) }
func (b *NotifyBuilder) Error(text string) error { return b.helper.sendTo(9, b.target, text) }
