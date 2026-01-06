package config

import (
	"fmt"
	"strings"
	"time"
)

func ParseDurationField(path, raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", path, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: duration must be >= 0", path)
	}
	return d, nil
}

func ParseDurationOrDefault(path, raw string, def time.Duration) (time.Duration, error) {
	d, err := ParseDurationField(path, raw)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return def, nil
	}
	return d, nil
}
