package pluginkit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pewbot/internal/core"
	"pewbot/internal/kit"
)

// BroadcastHelper wraps core.BroadcasterPort with a small builder API.
type BroadcastHelper struct {
	pluginName string
	svc        core.BroadcasterPort
	ctx        context.Context
}

func NewBroadcastHelper(pluginName string, services *core.Services) *BroadcastHelper {
	var svc core.BroadcasterPort
	if services != nil {
		svc = services.Broadcaster
	}
	return &BroadcastHelper{pluginName: pluginName, svc: svc}
}

func (h *BroadcastHelper) bindContext(ctx context.Context) { h.ctx = ctx }

// To starts building a broadcast job.
func (h *BroadcastHelper) To(targets []kit.ChatTarget) *BroadcastBuilder {
	return &BroadcastBuilder{helper: h, targets: targets}
}

type BroadcastBuilder struct {
	helper  *BroadcastHelper
	targets []kit.ChatTarget
	name    string
	options *kit.SendOptions
}

func (b *BroadcastBuilder) WithName(name string) *BroadcastBuilder {
	b.name = name
	return b
}

func (b *BroadcastBuilder) Markdown() *BroadcastBuilder {
	if b.options == nil {
		b.options = &kit.SendOptions{}
	}
	b.options.ParseMode = "Markdown"
	return b
}

func (b *BroadcastBuilder) HTML() *BroadcastBuilder {
	if b.options == nil {
		b.options = &kit.SendOptions{}
	}
	b.options.ParseMode = "HTML"
	return b
}

// Send creates and starts the broadcast job.
func (b *BroadcastBuilder) Send(text string) (string, error) {
	if b == nil || b.helper == nil || b.helper.svc == nil {
		return "", errors.New("broadcaster not available")
	}

	name := b.name
	if name == "" {
		name = fmt.Sprintf("broadcast-%d", time.Now().UnixNano())
	}
	// Namespace to reduce collisions in logs/metrics.
	if b.helper.pluginName != "" {
		name = b.helper.pluginName + ":" + name
	}

	jobID := b.helper.svc.NewJob(name, b.targets, text, b.options)
	ctx := b.helper.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return jobID, b.helper.svc.StartJob(ctx, jobID)
}
