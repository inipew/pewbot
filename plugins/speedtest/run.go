package speedtest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

func (p *Plugin) runSpeedtest(ctx context.Context) (*SpeedtestResult, string, error) {
	startTime := time.Now()

	// Create timeout context
	p.mu.RLock()
	timeout := p.cfg.operationTimeout
	serverCount := p.cfg.ServerCount
	p.mu.RUnlock()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fetch user info
	p.Log.Info("Fetching user info")
	user, err := speedtest.FetchUserInfo()
	if err != nil {
		return nil, "", fmt.Errorf("fetch user info: %w", err)
	}

	// Fetch servers
	p.Log.Info("Fetching servers")
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

	p.Log.Info("Speedtest completed",
		slog.Float64("download_mbps", result.DownloadMbps),
		slog.Float64("upload_mbps", result.UploadMbps),
		slog.Float64("ping_ms", result.PingMs),
		slog.Float64("duration_sec", duration.Seconds()),
	)

	return result, msg, nil
}

// saveResult saves speedtest result to history file
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

			p.Log.Debug("Testing server",
				slog.String("name", s.Name),
				slog.String("country", s.Country),
			)

			// Ping test
			if err := s.PingTest(nil); err != nil {
				p.Log.Warn("Ping test failed",
					slog.String("server", s.Name),
					slog.String("error", err.Error()),
				)
				return
			}

			// Download test
			if err := s.DownloadTest(); err != nil {
				p.Log.Warn("Download test failed",
					slog.String("server", s.Name),
					slog.String("error", err.Error()),
				)
				return
			}

			// Upload test
			if err := s.UploadTest(); err != nil {
				p.Log.Warn("Upload test failed",
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
