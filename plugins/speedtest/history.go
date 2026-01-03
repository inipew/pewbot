package speedtest

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// History file schema (NDJSON / JSON Lines)
//
// Each line is a single JSON object (one result), e.g.:
//   {"v":1,"id":"186d2a5c1b8a2d00-9f5a7c3b","timestamp":"2026-01-03T01:23:45Z","download_mbps":123.45,...}
//
// This format is:
//   - cheap to append (O(1))
//   - easy for bots/tools to read & manage (read/edit/delete by line / by `id`)
//   - compactable (rewrite file keeping the most recent N / last X days)

const (
	historySchemaVersion     = 1
	defaultHistoryMaxRecords = 2000
	defaultHistoryMaxAgeDays = 90
	defaultHistoryMaxBytes   = 2 * 1024 * 1024 // 2MB
)

type historyRecord struct {
	V  int    `json:"v"`
	ID string `json:"id"`
	SpeedtestResult
}

func newHistoryID(ts time.Time) string {
	// ts makes ordering/debugging easier; rand suffix makes collisions extremely unlikely.
	var b [4]byte
	_, _ = rand.Read(b[:])
	r := binary.BigEndian.Uint32(b[:])
	return fmt.Sprintf("%x-%08x", ts.UnixNano(), r)
}

func (p *Plugin) saveResult(result *SpeedtestResult) error {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()
	if historyFile == "" || result == nil {
		return nil
	}

	p.historyMu.Lock()
	defer p.historyMu.Unlock()

	// Ensure directory exists (if configured with a path).
	if dir := filepath.Dir(historyFile); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}

	rec := historyRecord{V: historySchemaVersion, ID: newHistoryID(result.Timestamp), SpeedtestResult: *result}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal history record: %w", err)
	}

	f, err := os.OpenFile(historyFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open history file: %w", err)
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("append history record: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("close history file: %w", cerr)
	}

	// Auto-compact when file grows beyond a small size. Compaction is bounded (keeps last N and last X days).
	if st, err := os.Stat(historyFile); err == nil && st.Size() > defaultHistoryMaxBytes {
		_, _ = compactHistoryFileLocked(historyFile, time.Now(), defaultHistoryMaxAgeDays, defaultHistoryMaxRecords)
	}

	return nil
}

// getDailyStats calculates statistics for the last 24 hours.
// Errors are treated as "no data" to keep the command UX simple.
func (p *Plugin) getDailyStats() *DailyStats {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()
	if historyFile == "" {
		return &DailyStats{Period: "Last 24 hours", TestCount: 0}
	}

	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)

	p.historyMu.Lock()
	defer p.historyMu.Unlock()

	f, err := os.Open(historyFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &DailyStats{Period: "Last 24 hours", TestCount: 0}
		}
		return &DailyStats{Period: "Last 24 hours", TestCount: 0}
	}
	defer f.Close()

	stats := &DailyStats{Period: "Last 24 hours"}
	var totalDownload, totalUpload, totalPing, totalPacketLoss float64

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec historyRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Timestamp.Before(cutoff) {
			continue
		}

		stats.TestCount++
		totalDownload += rec.DownloadMbps
		totalUpload += rec.UploadMbps
		totalPing += rec.PingMs
		totalPacketLoss += rec.PacketLoss

		if stats.TestCount == 1 {
			stats.MaxDownload = rec.DownloadMbps
			stats.MinDownload = rec.DownloadMbps
			stats.MaxUpload = rec.UploadMbps
			stats.MinUpload = rec.UploadMbps
			stats.MaxPing = rec.PingMs
			stats.MinPing = rec.PingMs
			stats.FirstTest = rec.Timestamp
			stats.LastTest = rec.Timestamp
			continue
		}

		if rec.DownloadMbps > stats.MaxDownload {
			stats.MaxDownload = rec.DownloadMbps
		}
		if rec.DownloadMbps < stats.MinDownload {
			stats.MinDownload = rec.DownloadMbps
		}
		if rec.UploadMbps > stats.MaxUpload {
			stats.MaxUpload = rec.UploadMbps
		}
		if rec.UploadMbps < stats.MinUpload {
			stats.MinUpload = rec.UploadMbps
		}
		if rec.PingMs > stats.MaxPing {
			stats.MaxPing = rec.PingMs
		}
		if rec.PingMs < stats.MinPing {
			stats.MinPing = rec.PingMs
		}
		if rec.Timestamp.Before(stats.FirstTest) {
			stats.FirstTest = rec.Timestamp
		}
		if rec.Timestamp.After(stats.LastTest) {
			stats.LastTest = rec.Timestamp
		}
	}

	if stats.TestCount == 0 {
		return stats
	}

	count := float64(stats.TestCount)
	stats.AvgDownload = totalDownload / count
	stats.AvgUpload = totalUpload / count
	stats.AvgPing = totalPing / count
	stats.AvgPacketLoss = totalPacketLoss / count
	return stats
}

// getRecentResults returns the most recent N results (newest first).
func (p *Plugin) getRecentResults(n int) []SpeedtestResult {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()
	if historyFile == "" || n <= 0 {
		return []SpeedtestResult{}
	}
	if n > 50 {
		n = 50
	}

	p.historyMu.Lock()
	defer p.historyMu.Unlock()

	f, err := os.Open(historyFile)
	if err != nil {
		return []SpeedtestResult{}
	}
	defer f.Close()

	buf := make([]SpeedtestResult, 0, n)
	idx := 0
	full := false

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec historyRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}

		if len(buf) < n {
			buf = append(buf, rec.SpeedtestResult)
			continue
		}
		buf[idx] = rec.SpeedtestResult
		idx = (idx + 1) % n
		full = true
	}

	// Normalize to chronological oldest->newest.
	ordered := buf
	if full {
		ordered = append([]SpeedtestResult(nil), buf[idx:]...)
		ordered = append(ordered, buf[:idx]...)
	}

	// Reverse for newest->oldest.
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	return ordered
}

// cleanOldResults removes results older than specified days by compacting the file.
func (p *Plugin) cleanOldResults(days int) int {
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()
	if historyFile == "" {
		return 0
	}
	if days < 1 {
		days = 1
	}

	p.historyMu.Lock()
	defer p.historyMu.Unlock()

	removed, err := compactHistoryFileLocked(historyFile, time.Now(), days, defaultHistoryMaxRecords)
	if err != nil {
		return 0
	}
	return removed
}

func compactHistoryFileLocked(filename string, now time.Time, keepDays int, maxRecords int) (int, error) {
	if keepDays < 1 {
		keepDays = 1
	}
	if maxRecords <= 0 {
		maxRecords = defaultHistoryMaxRecords
	}

	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)

	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("open history file: %w", err)
	}
	defer f.Close()

	// Ring buffer for last maxRecords kept records.
	kept := make([]historyRecord, 0, maxRecords)
	idx := 0
	full := false
	total := 0

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		total++
		var rec historyRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		// Backward compatibility: accept old JSON array schema (if user migrated manually) is out of scope.
		if rec.Timestamp.Before(cutoff) {
			continue
		}
		if len(kept) < maxRecords {
			kept = append(kept, rec)
			continue
		}
		kept[idx] = rec
		idx = (idx + 1) % maxRecords
		full = true
	}

	// Normalize order oldest->newest.
	ordered := kept
	if full {
		ordered = append([]historyRecord(nil), kept[idx:]...)
		ordered = append(ordered, kept[:idx]...)
	}

	// Write compacted file.
	tmp := filename + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return 0, fmt.Errorf("open temp history file: %w", err)
	}
	bw := bufio.NewWriter(out)
	for _, rec := range ordered {
		if rec.V == 0 {
			rec.V = historySchemaVersion
		}
		b, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		_, _ = bw.Write(b)
		_ = bw.WriteByte('\n')
	}
	_ = bw.Flush()
	_ = out.Sync()
	cerr := out.Close()
	if cerr != nil {
		return 0, fmt.Errorf("close temp history file: %w", cerr)
	}
	if err := os.Rename(tmp, filename); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("replace history file: %w", err)
	}

	removed := total - len(ordered)
	if removed < 0 {
		removed = 0
	}
	return removed, nil
}
