package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	logx "pewbot/pkg/logx"
	"strings"
	"sync"
	"time"
)

// fileStore is a dependency-free persistence backend.
//
// Files:
//   - <prefix>.audit.jsonl         (append-only JSON Lines)
//   - <prefix>.dedup.snapshot.json (periodic snapshot)
//   - <prefix>.dedup.journal.jsonl (append-only journal)
//
// The journal is periodically compacted into the snapshot.
type fileStore struct {
	log logx.Logger

	mu sync.Mutex

	auditFile *os.File

	dedupSnapshotPath string
	dedupJournalFile  *os.File
	dedup             map[string]int64 // unix milli

	dedupWrites int
}

type dedupRecord struct {
	Key   string `json:"key"`
	Until int64  `json:"until"`
}

func openFile(cfg Config, log logx.Logger) (Store, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, errors.New("storage.path is required for file driver")
	}
	if log.IsZero() {
		log = logx.Nop()
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	prefix := filepath.Join(dir, base)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	auditPath := prefix + ".audit.jsonl"
	snapPath := prefix + ".dedup.snapshot.json"
	journalPath := prefix + ".dedup.journal.jsonl"

	af, err := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}

	// Load dedup from snapshot + journal.
	dedup := map[string]int64{}
	_ = loadDedupSnapshot(snapPath, dedup)
	_ = replayDedupJournal(journalPath, dedup)
	pruneExpiredDedup(dedup)

	jf, err := os.OpenFile(journalPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		_ = af.Close()
		return nil, err
	}

	return &fileStore{
		log:               log,
		auditFile:         af,
		dedupSnapshotPath: snapPath,
		dedupJournalFile:  jf,
		dedup:             dedup,
		dedupWrites:       0,
	}, nil
}

func (s *fileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err1, err2 error
	if s.auditFile != nil {
		err1 = s.auditFile.Close()
		s.auditFile = nil
	}
	if s.dedupJournalFile != nil {
		err2 = s.dedupJournalFile.Close()
		s.dedupJournalFile = nil
	}
	if err1 != nil {
		return err1
	}
	return err2
}

func (s *fileStore) AppendAudit(ctx context.Context, e AuditEntry) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auditFile == nil {
		return errors.New("audit file closed")
	}
	enc := json.NewEncoder(s.auditFile)
	if err := enc.Encode(e); err != nil {
		return err
	}
	return nil
}

func (s *fileStore) PutDedup(ctx context.Context, key string, until time.Time) error {
	_ = ctx
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	ms := until.UnixMilli()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dedupJournalFile == nil {
		return errors.New("dedup journal closed")
	}
	if s.dedup == nil {
		s.dedup = map[string]int64{}
	}
	s.dedup[key] = ms

	// Append journal record.
	enc := json.NewEncoder(s.dedupJournalFile)
	if err := enc.Encode(dedupRecord{Key: key, Until: ms}); err != nil {
		return err
	}
	s.dedupWrites++
	if s.dedupWrites%1000 == 0 {
		// Best-effort compact.
		if err := s.compactLocked(); err != nil {
			s.log.Debug("dedup compact failed", logx.Any("err", err))
		}
	}
	return nil
}

func (s *fileStore) GetDedup(ctx context.Context, key string) (time.Time, bool, error) {
	_ = ctx
	key = strings.TrimSpace(key)
	if key == "" {
		return time.Time{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dedup == nil {
		return time.Time{}, false, nil
	}
	ms, ok := s.dedup[key]
	if !ok {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(ms), true, nil
}

func (s *fileStore) compactLocked() error {
	if s.dedup == nil {
		return nil
	}
	pruneExpiredDedup(s.dedup)

	tmp := s.dedupSnapshotPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(s.dedup); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.dedupSnapshotPath); err != nil {
		return err
	}
	// Truncate journal.
	if err := s.dedupJournalFile.Truncate(0); err != nil {
		return err
	}
	_, err = s.dedupJournalFile.Seek(0, 2)
	return err
}

func loadDedupSnapshot(path string, out map[string]int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var m map[string]int64
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return err
	}
	for k, v := range m {
		out[k] = v
	}
	return nil
}

func replayDedupJournal(path string, out map[string]int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		var r dedupRecord
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			continue
		}
		if r.Key == "" {
			continue
		}
		out[r.Key] = r.Until
	}
	return s.Err()
}

func pruneExpiredDedup(m map[string]int64) {
	now := time.Now().UnixMilli()
	for k, v := range m {
		if v < now {
			delete(m, k)
		}
	}
}
