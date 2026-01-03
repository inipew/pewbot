package speedtest

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestCompactHistoryBounds(t *testing.T) {
	now := time.Now()
	results := make([]SpeedtestResult, 0, historyMaxRecords+20)
	for i := 0; i < historyMaxRecords+20; i++ {
		results = append(results, SpeedtestResult{Timestamp: now.Add(-time.Duration(i) * time.Minute)})
	}
	results = append(results, SpeedtestResult{Timestamp: now.Add(-historyMaxAge - time.Hour)})

	compact := compactHistory(results)
	if len(compact) != historyMaxRecords {
		t.Fatalf("expected %d records, got %d", historyMaxRecords, len(compact))
	}
	if compact[0].Timestamp.Before(now.Add(-historyMaxAge)) {
		t.Fatalf("found record older than max age: %v", compact[0].Timestamp)
	}
	if !compact[0].Timestamp.Before(compact[len(compact)-1].Timestamp) {
		t.Fatalf("expected ascending timestamps")
	}
}

func TestHistoryFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "history.json")

	p := &Plugin{}
	p.Log = discardLogger()
	p.mu.Lock()
	p.cfg = Config{HistoryFile: file}
	p.mu.Unlock()

	now := time.Now()
	r1 := &SpeedtestResult{Timestamp: now.Add(-2 * time.Hour), DownloadMbps: 100, UploadMbps: 20, PingMs: 10}
	if err := p.saveResult(r1); err != nil {
		t.Fatalf("saveResult: %v", err)
	}

	r2 := &SpeedtestResult{Timestamp: now.Add(-26 * time.Hour), DownloadMbps: 90, UploadMbps: 15, PingMs: 12}
	p.histMu.Lock()
	if err := p.writeHistoryFile(file, []SpeedtestResult{*r1, *r2}); err != nil {
		t.Fatalf("writeHistoryFile: %v", err)
	}
	p.histMu.Unlock()

	removed, err := p.cleanOldResults(1)
	if err != nil {
		t.Fatalf("cleanOldResults: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed record, got %d", removed)
	}

	recent, err := p.getRecentResults(2)
	if err != nil {
		t.Fatalf("getRecentResults: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("expected 1 recent result, got %d", len(recent))
	}
	if recent[0].DownloadMbps != r1.DownloadMbps {
		t.Fatalf("unexpected data in recent result: %+v", recent[0])
	}

	stats := p.getDailyStats()
	if stats.TestCount != 1 {
		t.Fatalf("expected TestCount=1, got %d", stats.TestCount)
	}
	if stats.AvgDownload != r1.DownloadMbps {
		t.Fatalf("unexpected AvgDownload, got %f", stats.AvgDownload)
	}
}
