//go:build !sqlite
// +build !sqlite

package storage

import (
	"errors"
	logx "pewbot/pkg/logx"
)

func openSQLite(cfg Config, log logx.Logger) (Store, error) {
	_ = cfg
	_ = log
	return nil, errors.New("sqlite storage not built: build with -tags sqlite (and add a sqlite driver dependency)")
}
