package app

import (
	"fmt"
	"strings"
	"time"

	"pewbot/internal/storage"
)

func mapStorageConfig(cfg *Config) (storage.Config, bool, error) {
	if cfg == nil || cfg.Storage == nil {
		return storage.Config{}, false, nil
	}
	sc := cfg.Storage
	driver := strings.TrimSpace(sc.Driver)
	if driver == "" || strings.EqualFold(driver, "none") {
		return storage.Config{}, false, nil
	}
	path := strings.TrimSpace(sc.Path)

	dl := strings.ToLower(strings.TrimSpace(driver))
	switch dl {
	case "file":
		return storage.Config{Driver: "file", Path: path}, true, nil
	case "sqlite", "sqlite3":
		if path == "" {
			return storage.Config{}, false, fmt.Errorf("storage.path is required when storage.driver=sqlite")
		}
		busy := 1 * time.Second
		var err error
		busy, err = parseDurationOrDefault("storage.busy_timeout", sc.BusyTimeout, busy)
		if err != nil {
			return storage.Config{}, false, err
		}
		return storage.Config{Driver: dl, Path: path, BusyTimeout: busy}, true, nil
	default:
		return storage.Config{}, false, fmt.Errorf("unknown storage.driver: %s", driver)
	}
}
