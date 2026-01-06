package speedtest

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// NDJSON history schema version.
const historySchemaVersion = 1

const (
	DefaultHistoryMaxRecords = 2000
	DefaultHistoryMaxAgeDays = 90
	DefaultHistoryMaxBytes   = 2 * 1024 * 1024 // 2MB
)

type historyRecord struct {
	V  int    `json:"v"`
	ID string `json:"id"`
	Result
}

func newHistoryID(ts time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	r := binary.BigEndian.Uint32(b[:])
	return fmt.Sprintf("%x-%08x", ts.UnixNano(), r)
}

// HistoryStore persists speedtest results to a JSONL/NDJSON file.
//
// It is safe for concurrent use.
type HistoryStore struct {
	Filename   string
	MaxRecords int
	MaxAgeDays int
	MaxBytes   int64

	mu sync.Mutex
}

func NewHistoryStore(filename string) *HistoryStore {
	return &HistoryStore{
		Filename:   filename,
		MaxRecords: DefaultHistoryMaxRecords,
		MaxAgeDays: DefaultHistoryMaxAgeDays,
		MaxBytes:   DefaultHistoryMaxBytes,
	}
}

// Append adds a new result to the history file.
//
// When the file grows beyond MaxBytes, it is compacted best-effort.
func (h *HistoryStore) Append(result *Result) error {
	if h == nil || h.Filename == "" || result == nil {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Ensure directory exists.
	if dir := filepath.Dir(h.Filename); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}

	rec := historyRecord{V: historySchemaVersion, ID: newHistoryID(result.Timestamp), Result: *result}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal history record: %w", err)
	}

	f, err := os.OpenFile(h.Filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
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

	// Best-effort auto-compact.
	if h.MaxBytes > 0 {
		if st, err := os.Stat(h.Filename); err == nil && st.Size() > h.MaxBytes {
			_, _ = compactHistoryFileLocked(h.Filename, time.Now(), h.MaxAgeDays, h.MaxRecords)
		}
	}
	return nil
}

// Recent returns the most recent n results (newest first).
func (h *HistoryStore) Recent(n int) []Result {
	if h == nil || h.Filename == "" || n <= 0 {
		return nil
	}
	if n > 50 {
		n = 50
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	f, err := os.Open(h.Filename)
	if err != nil {
		return nil
	}
	defer f.Close()

	buf := make([]Result, 0, n)
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
			buf = append(buf, rec.Result)
			continue
		}
		buf[idx] = rec.Result
		idx = (idx + 1) % n
		full = true
	}

	ordered := buf
	if full {
		ordered = append([]Result(nil), buf[idx:]...)
		ordered = append(ordered, buf[:idx]...)
	}
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	return ordered
}

// Stats24h computes statistics for the last 24 hours.
//
// Errors are treated as "no data" (to keep bot UX simple).
func (h *HistoryStore) Stats24h() *DailyStats {
	if h == nil || h.Filename == "" {
		return &DailyStats{Period: "Last 24 hours", TestCount: 0}
	}

	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)

	h.mu.Lock()
	defer h.mu.Unlock()

	f, err := os.Open(h.Filename)
	if err != nil {
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

// CleanOlderThan removes results older than the given number of days and returns
// how many records were removed.
func (h *HistoryStore) CleanOlderThan(days int) int {
	if h == nil || h.Filename == "" {
		return 0
	}
	if days < 1 {
		days = 1
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	removed, err := compactHistoryFileLocked(h.Filename, time.Now(), days, h.MaxRecords)
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
		maxRecords = DefaultHistoryMaxRecords
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

	ordered := kept
	if full {
		ordered = append([]historyRecord(nil), kept[idx:]...)
		ordered = append(ordered, kept[:idx]...)
	}

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
