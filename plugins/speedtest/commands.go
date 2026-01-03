package speedtest

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"pewbot/internal/core"
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
	result, msg, err := p.runSpeedtest(ctx)
	if err != nil {
		errMsg := fmt.Sprintf("%sError: %v", prefix, err)
		_, _ = req.Adapter.SendText(ctx, req.Chat, errMsg, nil)
		return nil
	}

	// Save to history
	if err := p.saveResult(result); err != nil {
		p.Log.Warn("Failed to save result", slog.String("error", err.Error()))
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

	stats := p.getDailyStats()
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
	count := clampPositiveIntArg(req.Args, 5, 20)

	results, err := p.getRecentResults(count)
	if err != nil {
		p.Log.Warn("Failed to read history", slog.Any("err", err))
		_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+"Failed to read history", nil)
		return nil
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
	days := clampPositiveIntArg(req.Args, 30, 365)

	removed, err := p.cleanOldResults(days)
	if err != nil {
		p.Log.Warn("Failed to clean history", slog.Any("err", err))
		_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+"Failed to clean history: "+err.Error(), nil)
		return nil
	}

	msg := fmt.Sprintf("🧹 Cleaned %d results older than %d days", removed, days)
	_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+msg, nil)
	return nil
}

func clampPositiveIntArg(args []string, defVal, maxVal int) int {
	if len(args) == 0 {
		return defVal
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 {
		return defVal
	}
	if maxVal > 0 && n > maxVal {
		return maxVal
	}
	return n
}
