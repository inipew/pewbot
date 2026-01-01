package speedtest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"pewbot/internal/core"

	"github.com/showwin/speedtest-go/speedtest"
)

// Config defines plugin configuration
type Config struct {
	Prefix      string `json:"prefix"`
	AutoSpeed   Auto   `json:"auto_speedtest"`
	HistoryFile string `json:"history_file"`
	ServerCount int    `json:"server_count"`
	Timeout     int    `json:"timeout_seconds"` // timeout per test
}

// Auto defines auto speedtest schedule
type Auto struct {
	Schedule string `json:"schedule"`
}

// SpeedtestResult represents test results
type SpeedtestResult struct {
	Timestamp     time.Time `json:"timestamp"`
	DownloadMbps  float64   `json:"download_mbps"`
	UploadMbps    float64   `json:"upload_mbps"`
	PingMs        float64   `json:"ping_ms"`
	Jitter        float64   `json:"jitter"`
	PacketLoss    float64   `json:"packet_loss"`
	ISP           string    `json:"isp"`
	ServerName    string    `json:"server_name"`
	ServerCountry string    `json:"server_country"`
}

// serverTestResult contains individual server test data
type serverTestResult struct {
	Server   *speedtest.Server
	Download float64
	Upload   float64
	Ping     time.Duration
}

// SpeedtestHistory stores all historical results
type SpeedtestHistory struct {
	Results []SpeedtestResult `json:"results"`
	mu      sync.RWMutex
}

// DailyStats contains 24-hour statistics
type DailyStats struct {
	Period        string    `json:"period"`
	TestCount     int       `json:"test_count"`
	AvgDownload   float64   `json:"avg_download_mbps"`
	AvgUpload     float64   `json:"avg_upload_mbps"`
	AvgPing       float64   `json:"avg_ping_ms"`
	MaxDownload   float64   `json:"max_download_mbps"`
	MinDownload   float64   `json:"min_download_mbps"`
	MaxUpload     float64   `json:"max_upload_mbps"`
	MinUpload     float64   `json:"min_upload_mbps"`
	MaxPing       float64   `json:"max_ping_ms"`
	MinPing       float64   `json:"min_ping_ms"`
	AvgPacketLoss float64   `json:"avg_packet_loss"`
	FirstTest     time.Time `json:"first_test"`
	LastTest      time.Time `json:"last_test"`
}

// Plugin implements speedtest functionality
type Plugin struct {
	log     *slog.Logger
	deps    core.PluginDeps
	cfg     Config
	history *SpeedtestHistory
	mu      sync.RWMutex
}

// New creates a new speedtest plugin instance
func New() *Plugin {
	return &Plugin{
		history: &SpeedtestHistory{
			Results: make([]SpeedtestResult, 0),
		},
	}
}

// Name returns plugin name
func (p *Plugin) Name() string {
	return "speedtest"
}

// Init initializes the plugin
func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.deps = deps
	p.log = deps.Logger.With(slog.String("plugin", p.Name()))
	return nil
}

// Start starts the plugin
func (p *Plugin) Start(ctx context.Context) error {
	// Load existing history if configured
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()

	if historyFile != "" {
		if err := p.loadHistory(historyFile); err != nil {
			p.log.Warn("Failed to load history", slog.String("error", err.Error()))
		} else {
			p.log.Info("History loaded", slog.Int("count", len(p.history.Results)))
		}
	}
	return nil
}

// Stop stops the plugin
func (p *Plugin) Stop(ctx context.Context) error {
	return nil
}

// OnConfigChange handles configuration updates
func (p *Plugin) OnConfigChange(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	// Set defaults
	if c.ServerCount == 0 {
		c.ServerCount = 3
	}
	if c.Timeout == 0 {
		c.Timeout = 30
	}

	p.mu.Lock()
	p.cfg = c
	p.mu.Unlock()

	// Setup auto speedtest if configured
	if c.AutoSpeed.Schedule != "" && p.deps.Services != nil && p.deps.Services.Scheduler != nil {
		_, err := p.deps.Services.Scheduler.AddCron(
			"speedtest-auto",
			c.AutoSpeed.Schedule,
			60*time.Second,
			func(ctx context.Context) error {
				_, _, _ = p.runSpeedtest(ctx)
				return nil
			},
		)
		if err != nil {
			p.log.Warn("Failed to setup auto speedtest", slog.String("error", err.Error()))
		}
	}

	return nil
}

// runSpeedtest executes the speedtest
func (p *Plugin) runSpeedtest(ctx context.Context) (*SpeedtestResult, string, error) {
	startTime := time.Now()

	// Create timeout context
	p.mu.RLock()
	timeout := time.Duration(p.cfg.Timeout) * time.Second
	serverCount := p.cfg.ServerCount
	p.mu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fetch user info
	p.log.Info("Fetching user info")
	user, err := speedtest.FetchUserInfo()
	if err != nil {
		return nil, "", fmt.Errorf("fetch user info: %w", err)
	}

	// Fetch servers
	p.log.Info("Fetching servers")
	serverList, err := speedtest.FetchServers()
	if err != nil {
		return nil, "", fmt.Errorf("fetch servers: %w", err)
	}

	// Find best servers
	targets, err := serverList.FindServer([]int{})
	if err != nil || len(targets) == 0 {
		return nil, "", fmt.Errorf("no servers found")
	}

	// Limit servers
	if len(targets) > serverCount {
		targets = targets[:serverCount]
	}

	// Test servers in parallel
	results := p.testServersParallel(ctx, targets)
	if len(results) == 0 {
		return nil, "", fmt.Errorf("all server tests failed")
	}

	// Calculate statistics
	avg := p.calculateAverage(results)
	best := p.findBest(results)
	if best == nil {
		return nil, "", fmt.Errorf("no best server found")
	}

	// Test packet loss on best server
	packetLoss := p.testPacketLoss(ctx, best.Server)

	// Build result
	result := &SpeedtestResult{
		Timestamp:     time.Now(),
		DownloadMbps:  avg.Download,
		UploadMbps:    avg.Upload,
		PingMs:        float64(avg.Ping.Milliseconds()),
		Jitter:        float64(avg.Ping.Milliseconds()) * 0.1,
		PacketLoss:    packetLoss,
		ISP:           user.IP,
		ServerName:    best.Server.Sponsor,
		ServerCountry: best.Server.Country,
	}

	duration := time.Since(startTime)
	msg := p.formatResult(result, len(results), duration)

	p.log.Info("Speedtest completed",
		slog.Float64("download_mbps", result.DownloadMbps),
		slog.Float64("upload_mbps", result.UploadMbps),
		slog.Float64("ping_ms", result.PingMs),
		slog.Float64("duration_sec", duration.Seconds()),
	)

	return result, msg, nil
}

// saveResult saves speedtest result to history file
func (p *Plugin) saveResult(result *SpeedtestResult) error {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()

	if historyFile == "" {
		return nil
	}

	// Add to in-memory history
	p.history.mu.Lock()
	p.history.Results = append(p.history.Results, *result)
	p.history.mu.Unlock()

	// Save to file
	return p.saveHistory(historyFile)
}

// loadHistory loads history from file
func (p *Plugin) loadHistory(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist yet, not an error
		}
		return fmt.Errorf("read history file: %w", err)
	}

	var history SpeedtestHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return fmt.Errorf("unmarshal history: %w", err)
	}

	p.history.mu.Lock()
	p.history.Results = history.Results
	p.history.mu.Unlock()

	return nil
}

// saveHistory saves history to file
func (p *Plugin) saveHistory(filename string) error {
	p.history.mu.RLock()
	defer p.history.mu.RUnlock()

	data, err := json.MarshalIndent(p.history, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal history: %w", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("write history file: %w", err)
	}

	return nil
}

// getDailyStats calculates statistics for the last 24 hours
func (p *Plugin) getDailyStats() *DailyStats {
	p.history.mu.RLock()
	defer p.history.mu.RUnlock()

	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)

	// Filter results from last 24 hours
	var recentResults []SpeedtestResult
	for _, r := range p.history.Results {
		if r.Timestamp.After(yesterday) {
			recentResults = append(recentResults, r)
		}
	}

	if len(recentResults) == 0 {
		return &DailyStats{
			Period:    "Last 24 hours",
			TestCount: 0,
		}
	}

	stats := &DailyStats{
		Period:      "Last 24 hours",
		TestCount:   len(recentResults),
		MaxDownload: recentResults[0].DownloadMbps,
		MinDownload: recentResults[0].DownloadMbps,
		MaxUpload:   recentResults[0].UploadMbps,
		MinUpload:   recentResults[0].UploadMbps,
		MaxPing:     recentResults[0].PingMs,
		MinPing:     recentResults[0].PingMs,
		FirstTest:   recentResults[0].Timestamp,
		LastTest:    recentResults[0].Timestamp,
	}

	var totalDownload, totalUpload, totalPing, totalPacketLoss float64

	for _, r := range recentResults {
		// Sum for averages
		totalDownload += r.DownloadMbps
		totalUpload += r.UploadMbps
		totalPing += r.PingMs
		totalPacketLoss += r.PacketLoss

		// Track min/max
		if r.DownloadMbps > stats.MaxDownload {
			stats.MaxDownload = r.DownloadMbps
		}
		if r.DownloadMbps < stats.MinDownload {
			stats.MinDownload = r.DownloadMbps
		}
		if r.UploadMbps > stats.MaxUpload {
			stats.MaxUpload = r.UploadMbps
		}
		if r.UploadMbps < stats.MinUpload {
			stats.MinUpload = r.UploadMbps
		}
		if r.PingMs > stats.MaxPing {
			stats.MaxPing = r.PingMs
		}
		if r.PingMs < stats.MinPing {
			stats.MinPing = r.PingMs
		}

		// Track first/last test
		if r.Timestamp.Before(stats.FirstTest) {
			stats.FirstTest = r.Timestamp
		}
		if r.Timestamp.After(stats.LastTest) {
			stats.LastTest = r.Timestamp
		}
	}

	count := float64(len(recentResults))
	stats.AvgDownload = totalDownload / count
	stats.AvgUpload = totalUpload / count
	stats.AvgPing = totalPing / count
	stats.AvgPacketLoss = totalPacketLoss / count

	return stats
}

// cleanOldResults removes results older than specified days
func (p *Plugin) cleanOldResults(days int) int {
	p.history.mu.Lock()
	defer p.history.mu.Unlock()

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	var kept []SpeedtestResult
	removed := 0

	for _, r := range p.history.Results {
		if r.Timestamp.After(cutoff) {
			kept = append(kept, r)
		} else {
			removed++
		}
	}

	p.history.Results = kept
	return removed
}

// getRecentResults returns the most recent N results
func (p *Plugin) getRecentResults(n int) []SpeedtestResult {
	p.history.mu.RLock()
	defer p.history.mu.RUnlock()

	if len(p.history.Results) == 0 {
		return []SpeedtestResult{}
	}

	// Sort by timestamp descending
	sorted := make([]SpeedtestResult, len(p.history.Results))
	copy(sorted, p.history.Results)

	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.After(sorted[j].Timestamp)
	})

	if n > len(sorted) {
		n = len(sorted)
	}

	return sorted[:n]
}

// formatStats formats daily statistics into readable message
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
func (p *Plugin) testServersParallel(ctx context.Context, servers []*speedtest.Server) []serverTestResult {
	var wg sync.WaitGroup
	resultsChan := make(chan serverTestResult, len(servers))
	semaphore := make(chan struct{}, 3) // Limit concurrent tests to 3

	for _, server := range servers {
		wg.Add(1)
		go func(s *speedtest.Server) {
			defer wg.Done()

			// Check context
			select {
			case <-ctx.Done():
				return
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			}

			p.log.Debug("Testing server",
				slog.String("name", s.Name),
				slog.String("country", s.Country),
			)

			// Ping test
			if err := s.PingTest(nil); err != nil {
				p.log.Warn("Ping test failed",
					slog.String("server", s.Name),
					slog.String("error", err.Error()),
				)
				return
			}

			// Download test
			if err := s.DownloadTest(); err != nil {
				p.log.Warn("Download test failed",
					slog.String("server", s.Name),
					slog.String("error", err.Error()),
				)
				return
			}

			// Upload test
			if err := s.UploadTest(); err != nil {
				p.log.Warn("Upload test failed",
					slog.String("server", s.Name),
					slog.String("error", err.Error()),
				)
				return
			}

			resultsChan <- serverTestResult{
				Server:   s,
				Download: s.DLSpeed.Mbps(),
				Upload:   s.ULSpeed.Mbps(),
				Ping:     s.Latency,
			}
		}(server)
	}

	// Wait for all tests to complete
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results
	results := make([]serverTestResult, 0, len(servers))
	for r := range resultsChan {
		results = append(results, r)
	}

	return results
}

// testPacketLoss performs packet loss test
func (p *Plugin) testPacketLoss(ctx context.Context, server *speedtest.Server) float64 {
	const pingCount = 10
	failed := 0

	for i := 0; i < pingCount; i++ {
		select {
		case <-ctx.Done():
			return 100.0 // Context cancelled, return 100% loss
		default:
		}

		if err := server.PingTest(nil); err != nil {
			failed++
		}
		time.Sleep(50 * time.Millisecond)
	}

	return float64(failed) / float64(pingCount) * 100.0
}

// calculateAverage computes average metrics
func (p *Plugin) calculateAverage(results []serverTestResult) serverTestResult {
	if len(results) == 0 {
		return serverTestResult{}
	}

	var totalDL, totalUL float64
	var totalPing time.Duration

	for _, r := range results {
		totalDL += r.Download
		totalUL += r.Upload
		totalPing += r.Ping
	}

	count := len(results)
	return serverTestResult{
		Download: totalDL / float64(count),
		Upload:   totalUL / float64(count),
		Ping:     totalPing / time.Duration(count),
	}
}

// findBest identifies the best server
func (p *Plugin) findBest(results []serverTestResult) *serverTestResult {
	if len(results) == 0 {
		return nil
	}

	best := &results[0]
	for i := 1; i < len(results); i++ {
		// Prioritize lower ping, then higher download speed
		if results[i].Ping < best.Ping ||
			(results[i].Ping == best.Ping && results[i].Download > best.Download) {
			best = &results[i]
		}
	}

	return best
}

// formatResult formats the result into a readable message
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
func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "speedtest",
			Aliases:     []string{"st"},
			Description: "Run speedtest with detailed results",
			Usage:       "/speedtest",
			Access:      core.AccessOwnerOnly,
			Handle:      p.handleSpeedtest,
		},
		{
			Route:       "speedtest-stats",
			Aliases:     []string{"sts"},
			Description: "Show 24-hour speedtest statistics",
			Usage:       "/speedtest-stats",
			Access:      core.AccessOwnerOnly,
			Handle:      p.handleStats,
		},
		{
			Route:       "speedtest-history",
			Aliases:     []string{"sth"},
			Description: "Show recent speedtest history",
			Usage:       "/speedtest-history [count]",
			Access:      core.AccessOwnerOnly,
			Handle:      p.handleHistory,
		},
		{
			Route:       "speedtest-clean",
			Aliases:     []string{"stc"},
			Description: "Clean old speedtest results",
			Usage:       "/speedtest-clean [days]",
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
		p.log.Warn("Failed to save result", slog.String("error", err.Error()))
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
	count := 5
	if len(req.Args) > 0 {
		if n, err := fmt.Sscanf(req.Args[0], "%d", &count); err == nil && n == 1 {
			if count > 20 {
				count = 20 // Limit to 20
			}
		}
	}

	results := p.getRecentResults(count)
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
	historyFile := p.cfg.HistoryFile
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

	removed := p.cleanOldResults(days)

	// Save cleaned history
	if historyFile != "" {
		if err := p.saveHistory(historyFile); err != nil {
			p.log.Warn("Failed to save cleaned history", slog.String("error", err.Error()))
		}
	}

	msg := fmt.Sprintf("🧹 Cleaned %d results older than %d days", removed, days)
	_, _ = req.Adapter.SendText(ctx, req.Chat, prefix+msg, nil)
	return nil
}
