package speedtest

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Keep history small and readable: trim by age and count to avoid unbounded files.
const (
	historyMaxRecords = 500
	historyMaxAge     = 90 * 24 * time.Hour
)

func (p *Plugin) saveResult(result *SpeedtestResult) error {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()

	if historyFile == "" {
		return nil
	}

	p.histMu.Lock()
	defer p.histMu.Unlock()

	results, err := p.readHistoryFile(historyFile)
	if err != nil {
		return err
	}
	results = append(results, *result)
	results = compactHistory(results)
	return p.writeHistoryFile(historyFile, results)
}

func (p *Plugin) compactHistoryFile(filename string) (int, error) {
	p.histMu.Lock()
	defer p.histMu.Unlock()

	results, err := p.readHistoryFile(filename)
	if err != nil {
		return 0, err
	}
	if err := p.writeHistoryFile(filename, results); err != nil {
		return 0, err
	}
	return len(results), nil
}

func (p *Plugin) readHistoryFile(filename string) ([]SpeedtestResult, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return []SpeedtestResult{}, nil
		}
		return nil, fmt.Errorf("read history file: %w", err)
	}
	if len(data) == 0 {
		return []SpeedtestResult{}, nil
	}

	var results []SpeedtestResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("unmarshal history: %w", err)
	}
	return compactHistory(results), nil
}

func (p *Plugin) writeHistoryFile(filename string, results []SpeedtestResult) error {
	results = compactHistory(results)

	if dir := filepath.Dir(filename); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create history dir: %w", err)
		}
	}

	data, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("marshal history: %w", err)
	}

	if err := os.WriteFile(filename, data, 0o644); err != nil {
		return fmt.Errorf("write history file: %w", err)
	}
	return nil
}

func compactHistory(results []SpeedtestResult) []SpeedtestResult {
	if len(results) == 0 {
		return results
	}

	cutoff := time.Now().Add(-historyMaxAge)
	filtered := make([]SpeedtestResult, 0, len(results))
	for _, r := range results {
		if r.Timestamp.IsZero() {
			continue
		}
		if r.Timestamp.Before(cutoff) {
			continue
		}
		filtered = append(filtered, r)
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Timestamp.Before(filtered[j].Timestamp)
	})
	if len(filtered) > historyMaxRecords {
		filtered = filtered[len(filtered)-historyMaxRecords:]
	}
	return filtered
}

// getDailyStats calculates statistics for the last 24 hours.
func (p *Plugin) getDailyStats() *DailyStats {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()

	if historyFile == "" {
		return &DailyStats{Period: "Last 24 hours", TestCount: 0}
	}

	p.histMu.Lock()
	results, err := p.readHistoryFile(historyFile)
	p.histMu.Unlock()
	if err != nil {
		p.Log.Warn("Failed to read history for stats", slog.Any("err", err))
		return &DailyStats{Period: "Last 24 hours", TestCount: 0}
	}

	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)

	var recentResults []SpeedtestResult
	for _, r := range results {
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
		totalDownload += r.DownloadMbps
		totalUpload += r.UploadMbps
		totalPing += r.PingMs
		totalPacketLoss += r.PacketLoss

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

// cleanOldResults removes results older than specified days.
func (p *Plugin) cleanOldResults(days int) (int, error) {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()

	if historyFile == "" {
		return 0, nil
	}

	p.histMu.Lock()
	defer p.histMu.Unlock()

	results, err := p.readHistoryFile(historyFile)
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	kept := make([]SpeedtestResult, 0, len(results))
	removed := 0

	for _, r := range results {
		if r.Timestamp.Before(cutoff) {
			removed++
			continue
		}
		kept = append(kept, r)
	}

	if removed > 0 {
		if err := p.writeHistoryFile(historyFile, kept); err != nil {
			return 0, err
		}
	}

	return removed, nil
}

// getRecentResults returns the most recent N results.
func (p *Plugin) getRecentResults(n int) ([]SpeedtestResult, error) {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()

	if historyFile == "" {
		return []SpeedtestResult{}, nil
	}

	p.histMu.Lock()
	results, err := p.readHistoryFile(historyFile)
	p.histMu.Unlock()
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return []SpeedtestResult{}, nil
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.After(results[j].Timestamp)
	})

	if n > len(results) {
		n = len(results)
	}

	return results[:n], nil
}
