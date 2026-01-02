package speedtest

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

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
