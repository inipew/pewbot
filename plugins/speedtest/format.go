package speedtest

import (
	"fmt"
	"time"
)

func (p *Plugin) formatStats(stats *DailyStats) string {
	if stats.TestCount == 0 {
		return "📊 No speedtest data available for the last 24 hours"
	}

	return fmt.Sprintf(
		"📊 24-Hour Speedtest Statistics\n"+
			"━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
			"📈 Tests: %d\n"+
			"⏰ Period: %s → %s\n\n"+
			"⬇️  Download:\n"+
			"   • Average: %.2f Mbps\n"+
			"   • Maximum: %.2f Mbps\n"+
			"   • Minimum: %.2f Mbps\n\n"+
			"⬆️  Upload:\n"+
			"   • Average: %.2f Mbps\n"+
			"   • Maximum: %.2f Mbps\n"+
			"   • Minimum: %.2f Mbps\n\n"+
			"📡 Ping:\n"+
			"   • Average: %.2f ms\n"+
			"   • Maximum: %.2f ms\n"+
			"   • Minimum: %.2f ms\n\n"+
			"📦 Packet Loss: %.2f%%",
		stats.TestCount,
		stats.FirstTest.Format("15:04:05"),
		stats.LastTest.Format("15:04:05"),
		stats.AvgDownload,
		stats.MaxDownload,
		stats.MinDownload,
		stats.AvgUpload,
		stats.MaxUpload,
		stats.MinUpload,
		stats.AvgPing,
		stats.MaxPing,
		stats.MinPing,
		stats.AvgPacketLoss,
	)
}

// testServersParallel tests multiple servers concurrently
func (p *Plugin) formatResult(res *SpeedtestResult, serverCount int, duration time.Duration) string {
	return fmt.Sprintf(
		"🚀 Speedtest Results\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"⬇️  Download: %.2f Mbps\n"+
			"⬆️  Upload: %.2f Mbps\n"+
			"📡 Ping: %.2f ms\n"+
			"📊 Jitter: %.2f ms\n"+
			"📦 Packet Loss: %.2f%%\n"+
			"🌐 ISP: %s\n"+
			"🖥️  Server: %s (%s)\n"+
			"⏱️  Duration: %.1fs | Servers: %d\n"+
			"🕐 Time: %s",
		res.DownloadMbps,
		res.UploadMbps,
		res.PingMs,
		res.Jitter,
		res.PacketLoss,
		res.ISP,
		res.ServerName,
		res.ServerCountry,
		duration.Seconds(),
		serverCount,
		res.Timestamp.Format("2006-01-02 15:04:05"),
	)
}

// Commands returns available commands
