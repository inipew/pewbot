//go:build !sqlite
// +build !sqlite

package storage

import (
	"errors"
	"log/slog"
)

func openSQLite(cfg Config, log *slog.Logger) (Store, error) {
	_ = cfg
	_ = log
	return nil, errors.New("sqlite storage not built: build with -tags sqlite (and add a sqlite driver dependency)")
}
