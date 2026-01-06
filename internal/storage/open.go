package storage

import (
	"context"
	"errors"
	logx "pewbot/pkg/logx"
	"strings"
	"time"
)

// Store is the minimal persistence API used by core/services.
type Store interface {
	AppendAudit(ctx context.Context, e AuditEntry) error
	PutDedup(ctx context.Context, key string, until time.Time) error
	GetDedup(ctx context.Context, key string) (until time.Time, ok bool, err error)
	Close() error
}

// Open initializes the configured store.
// It returns (nil, nil) if storage is disabled.
func Open(cfg Config, log logx.Logger) (Store, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	if driver == "" || driver == "none" {
		return nil, nil
	}
	if log.IsZero() {
		log = logx.Nop()
	}

	switch driver {
	case "file":
		return openFile(cfg, log)
	case "sqlite", "sqlite3":
		return openSQLite(cfg, log)
	default:
		return nil, errors.New("unknown storage driver: " + driver)
	}
}
