package storage

import (
	"errors"
	"time"
)

var ErrDisabled = errors.New("storage disabled")

// Config configures storage.
//
// Driver values:
//   - "file": dependency-free file backend (jsonl + snapshot)
//   - "sqlite": SQLite database file (optional build tag)
//
// If Driver is empty or "none", storage is disabled.
type Config struct {
	Driver      string
	Path        string
	BusyTimeout time.Duration // sqlite only; 0 means default
}

// AuditEntry records an operator action.
// Keep it compact and schema-stable.
type AuditEntry struct {
	At            time.Time
	ActorID       int64
	ActorUsername string
	ChatID        int64
	ThreadID      int
	Plugin        string
	Action        string
	Target        string
	OK            int
	Fail          int
	Error         string
	TookMS        int64
	MetaJSON      string
}
