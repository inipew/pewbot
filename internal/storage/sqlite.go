//go:build sqlite
// +build sqlite

package storage

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	logx "pewbot/pkg/logx"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations.sql
var migrationsFS embed.FS

type sqliteStore struct {
	db  *sql.DB
	log logx.Logger

	opCount    atomic.Uint64
	pruneEvery uint64
}

func openSQLite(cfg Config, log logx.Logger) (Store, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("sqlite path is required")
	}
	path := cfg.Path
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite prefers a small number of concurrent writers.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	st := &sqliteStore{db: db, log: log, pruneEvery: 500}

	// Basic pragmas.
	if cfg.BusyTimeout > 0 {
		ms := cfg.BusyTimeout.Milliseconds()
		_, _ = db.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", ms))
	}
	_, _ = db.Exec("PRAGMA journal_mode = WAL")
	_, _ = db.Exec("PRAGMA synchronous = NORMAL")

	if err := st.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

func (s *sqliteStore) migrate(ctx context.Context) error {
	b, err := migrationsFS.ReadFile("migrations.sql")
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, string(b))
	return err
}

func (s *sqliteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *sqliteStore) AppendAudit(ctx context.Context, e AuditEntry) error {
	if s == nil || s.db == nil {
		return ErrDisabled
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit(at, actor_id, actor_username, chat_id, thread_id, plugin, action, target, ok, fail, err, took_ms, meta)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.At.Format(time.RFC3339Nano), e.ActorID, nullStr(e.ActorUsername), e.ChatID, e.ThreadID,
		e.Plugin, e.Action, e.Target, e.OK, e.Fail, nullStr(e.Error), e.TookMS, nullStr(e.MetaJSON),
	)
	return err
}

func (s *sqliteStore) PutDedup(ctx context.Context, key string, until time.Time) error {
	if s == nil || s.db == nil {
		return ErrDisabled
	}
	if key == "" {
		return nil
	}
	ms := until.UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dedup(key, until) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET until=excluded.until`,
		key, ms,
	)
	if err == nil && s.opCount.Add(1)%s.pruneEvery == 0 {
		pctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_ = s.pruneExpired(pctx)
		cancel()
	}
	return err
}

func (s *sqliteStore) GetDedup(ctx context.Context, key string) (time.Time, bool, error) {
	if s == nil || s.db == nil {
		return time.Time{}, false, ErrDisabled
	}
	if key == "" {
		return time.Time{}, false, nil
	}
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT until FROM dedup WHERE key = ?`, key).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.UnixMilli(ms), true, nil
}

func (s *sqliteStore) pruneExpired(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `DELETE FROM dedup WHERE until < ?`, now)
	return err
}

func nullStr(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
