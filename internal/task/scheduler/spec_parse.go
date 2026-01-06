package scheduler

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SpecKind describes the normalized kind of a schedule string.
//
// We intentionally keep this small: either a cron expression (robfig/cron)
// or a fixed interval.
type SpecKind int

const (
	SpecCron SpecKind = iota
	SpecInterval
)

// ParsedSpec represents a parsed schedule string.
//
// Supported forms:
//   - Cron (crontab.guru-style): "*/5 * * * *", "55 * * * *", "@hourly", "@every 55m"
//   - Interval duration: "55m", "2h30m"
//   - Interval HH:MM: "00:50" (50 minutes), "02:30" (2 hours 30 minutes)
//
// Optional prefixes:
//   - "cron:" forces cron parsing
//   - "interval:" or "every:" forces interval parsing
type ParsedSpec struct {
	Kind   SpecKind
	Cron   string
	Every  time.Duration
	Source string // "cron" | "duration" | "hhmm"
}

var reHHMM = regexp.MustCompile(`^\s*(\d{1,3}):(\d{2})\s*$`)

// ParseSchedule parses a schedule string into either a cron expression or an interval duration.
func ParseSchedule(raw string) (ParsedSpec, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ParsedSpec{}, fmt.Errorf("schedule required")
	}

	// Prefixes (explicit)
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "cron:") {
		expr := strings.TrimSpace(s[len("cron:"):])
		if expr == "" {
			return ParsedSpec{}, fmt.Errorf("cron schedule required after 'cron:'")
		}
		return ParsedSpec{Kind: SpecCron, Cron: expr, Source: "cron"}, nil
	}
	if strings.HasPrefix(low, "interval:") {
		v := strings.TrimSpace(s[len("interval:"):])
		d, src, err := parseInterval(v)
		if err != nil {
			return ParsedSpec{}, err
		}
		return ParsedSpec{Kind: SpecInterval, Every: d, Source: src}, nil
	}
	if strings.HasPrefix(low, "every:") {
		v := strings.TrimSpace(s[len("every:"):])
		d, src, err := parseInterval(v)
		if err != nil {
			return ParsedSpec{}, err
		}
		return ParsedSpec{Kind: SpecInterval, Every: d, Source: src}, nil
	}

	// Heuristics:
	// - any whitespace or leading '@' => cron
	if strings.ContainsAny(s, " \t\n\r") || strings.HasPrefix(s, "@") {
		return ParsedSpec{Kind: SpecCron, Cron: s, Source: "cron"}, nil
	}

	// - HH:MM => interval duration
	if reHHMM.MatchString(s) {
		d, _, err := parseHHMMDuration(s)
		if err != nil {
			return ParsedSpec{}, err
		}
		return ParsedSpec{Kind: SpecInterval, Every: d, Source: "hhmm"}, nil
	}

	// - Go duration => interval duration
	d, err := time.ParseDuration(s)
	if err == nil {
		if d <= 0 {
			return ParsedSpec{}, fmt.Errorf("interval must be > 0")
		}
		return ParsedSpec{Kind: SpecInterval, Every: d, Source: "duration"}, nil
	}

	return ParsedSpec{}, fmt.Errorf(
		"invalid schedule %q (use cron like '*/5 * * * *', HH:MM like '02:30', or duration like '55m')",
		raw,
	)
}

func parseInterval(v string) (time.Duration, string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, "", fmt.Errorf("interval required")
	}
	if reHHMM.MatchString(v) {
		d, _, err := parseHHMMDuration(v)
		return d, "hhmm", err
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, "", fmt.Errorf("invalid interval %q (use HH:MM or Go duration like '55m'/'2h30m')", v)
	}
	if d <= 0 {
		return 0, "", fmt.Errorf("interval must be > 0")
	}
	return d, "duration", nil
}

func parseHHMMDuration(v string) (time.Duration, string, error) {
	m := reHHMM.FindStringSubmatch(v)
	if len(m) != 3 {
		return 0, "", fmt.Errorf("invalid HH:MM %q", v)
	}
	// safe parse: hours up to 999, minutes 0..59
	var hh int
	for i := 0; i < len(m[1]); i++ {
		hh = hh*10 + int(m[1][i]-'0')
	}
	var mm int
	mm = int(m[2][0]-'0')*10 + int(m[2][1]-'0')
	if mm < 0 || mm > 59 {
		return 0, "", fmt.Errorf("invalid minutes in %q", v)
	}
	d := time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute
	if d <= 0 {
		return 0, "", fmt.Errorf("interval must be > 0")
	}
	return d, "hhmm", nil
}
