package notify

import (
	"context"
	"log/slog"
	"sync"

	"pewbot/internal/kit"
)

type Service struct {
	adapter kit.Adapter
	log     *slog.Logger

	mu      sync.Mutex
	history []kit.Notification
}

func New(adapter kit.Adapter, log *slog.Logger) *Service {
	return &Service{adapter: adapter, log: log}
}

func (n *Service) Notify(ctx context.Context, noti kit.Notification) error {
	if noti.Channel == "" {
		noti.Channel = "telegram"
	}
	if noti.Options == nil {
		noti.Options = &kit.SendOptions{ParseMode: "", DisablePreview: true}
	}

	prefix := ""
	switch {
	case noti.Priority >= 8:
		prefix = "🚨 "
	case noti.Priority >= 5:
		prefix = "⚠️ "
	default:
		prefix = "ℹ️ "
	}
	_, err := n.adapter.SendText(ctx, noti.Target, prefix+noti.Text, noti.Options)
	n.appendHistory(noti)
	return err
}

func (n *Service) appendHistory(x kit.Notification) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.history = append(n.history, x)
	if len(n.history) > 300 {
		n.history = n.history[len(n.history)-300:]
	}
}

func (n *Service) Stop(ctx context.Context) {
	// no background workers currently
}
