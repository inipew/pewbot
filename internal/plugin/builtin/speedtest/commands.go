package speedtest

import (
	"context"
	"fmt"
	core "pewbot/internal/plugin"
	logx "pewbot/pkg/logx"

	speedpkg "pewbot/pkg/speedtest"
)

func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "speedtest",
			Aliases:     []string{"st"},
			Description: "jalankan speedtest (hasil detail)",
			Usage:       "/speedtest",
			Access:      core.AccessOwnerOnly,
			Handle:      p.handleSpeedtest,
		},
		{
			Route:       "speedtest stats",
			Aliases:     []string{"sts", "speedtest-stats"},
			Description: "statistik speedtest 24 jam terakhir",
			Usage:       "/speedtest stats",
			Access:      core.AccessOwnerOnly,
			Handle:      p.handleStats,
		},
		{
			Route:       "speedtest history",
			Aliases:     []string{"sth", "speedtest-history"},
			Description: "riwayat speedtest terbaru",
			Usage:       "/speedtest history [count]",
			Access:      core.AccessOwnerOnly,
			Handle:      p.handleHistory,
		},
		{
			Route:       "speedtest clean",
			Aliases:     []string{"stc", "speedtest-clean"},
			Description: "bersihkan hasil speedtest lama",
			Usage:       "/speedtest clean [days]",
			Access:      core.AccessOwnerOnly,
			Handle:      p.handleClean,
		},
	}
}

// handleSpeedtest handles the speedtest command
func (p *Plugin) handleSpeedtest(ctx context.Context, req *core.Request) error {
	p.mu.RLock()
	prefix := p.cfg.Prefix
	p.mu.RUnlock()

	// Send loading message
	_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+"Running speedtest, please wait...", nil)

	// Run speedtest
	result, msg, err := p.runSpeedtest(ctx, "command")
	if err == ErrAlreadyRunning {
		_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+"Speedtest sedang berjalan, coba lagi sebentar lagi.", nil)
		return nil
	}
	if err != nil {
		errMsg := fmt.Sprintf("%sError: %v", prefix, err)
		_, _ = req.Adapter.SendText(ctx, req.Chat, errMsg, nil)
		return nil
	}

	// Save to history (best-effort)
	if h := p.history(); h != nil {
		if err := h.Append(result); err != nil {
			p.Log.Warn("Failed to save result", logx.String("error", err.Error()))
		}
	}

	// Send result
	_, _ = req.Adapter.SendText(ctx, req.Chat, msg, nil)
	return nil
}

// handleStats handles the speedtest-stats command
func (p *Plugin) handleStats(ctx context.Context, req *core.Request) error {
	p.mu.RLock()
	prefix := p.cfg.Prefix
	p.mu.RUnlock()

	stats := (&speedpkg.DailyStats{Period: "Last 24 hours", TestCount: 0})
	if h := p.history(); h != nil {
		stats = h.Stats24h()
	}
	msg := p.formatStats(stats)

	_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+msg, nil)
	return nil
}

// handleHistory handles the speedtest-history command
func (p *Plugin) handleHistory(ctx context.Context, req *core.Request) error {
	p.mu.RLock()
	prefix := p.cfg.Prefix
	p.mu.RUnlock()

	// Default to 5 recent results
	count := 5
	if len(req.Args) > 0 {
		if n, err := fmt.Sscanf(req.Args[0], "%d", &count); err == nil && n == 1 {
			if count > 20 {
				count = 20 // Limit to 20
			}
		}
	}

	var results []speedpkg.Result
	if h := p.history(); h != nil {
		results = h.Recent(count)
	}
	if len(results) == 0 {
		_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+"No speedtest history available", nil)
		return nil
	}

	msg := fmt.Sprintf("📜 Recent %d Speedtest Results\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n", len(results))
	for i, r := range results {
		msg += fmt.Sprintf(
			"\n%d. %s\n"+
				"   ⬇️  %.2f Mbps | ⬆️  %.2f Mbps | 📡 %.2f ms\n"+
				"   📦 %.2f%% loss | 🖥️  %s",
			i+1,
			r.Timestamp.Format("2006-01-02 15:04:05"),
			r.DownloadMbps,
			r.UploadMbps,
			r.PingMs,
			r.PacketLoss,
			r.ServerName,
		)
	}

	_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+msg, nil)
	return nil
}

// handleClean handles the speedtest-clean command
func (p *Plugin) handleClean(ctx context.Context, req *core.Request) error {
	p.mu.RLock()
	prefix := p.cfg.Prefix
	p.mu.RUnlock()

	// Default to 30 days
	days := 30
	if len(req.Args) > 0 {
		if n, err := fmt.Sscanf(req.Args[0], "%d", &days); err == nil && n == 1 {
			if days < 1 {
				days = 1
			}
		}
	}

	removed := 0
	if h := p.history(); h != nil {
		removed = h.CleanOlderThan(days)
	}

	msg := fmt.Sprintf("🧹 Cleaned %d results older than %d days", removed, days)
	_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+msg, nil)
	return nil
}
