package speedtest

import (
	"fmt"

	speedpkg "pewbot/pkg/speedtest"
)

func (p *Plugin) formatStats(stats *speedpkg.DailyStats) string {
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
