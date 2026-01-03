package scheduler

import (
	"testing"
	"time"
)

func TestParseScheduleVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		kind     SpecKind
		source   string
		duration time.Duration
	}{
		{name: "cron", raw: "*/5 * * * *", kind: SpecCron, source: "cron"},
		{name: "prefixed cron", raw: "cron:0 0 * * *", kind: SpecCron, source: "cron"},
		{name: "duration", raw: "10m", kind: SpecInterval, source: "duration", duration: 10 * time.Minute},
		{name: "prefixed interval", raw: "interval:45s", kind: SpecInterval, source: "duration", duration: 45 * time.Second},
		{name: "hhmm", raw: "01:30", kind: SpecInterval, source: "hhmm", duration: 90 * time.Minute},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSchedule(tt.raw)
			if err != nil {
				t.Fatalf("ParseSchedule(%q) error: %v", tt.raw, err)
			}
			if got.Kind != tt.kind {
				t.Fatalf("Kind = %v, want %v", got.Kind, tt.kind)
			}
			if got.Source != tt.source {
				t.Fatalf("Source = %s, want %s", got.Source, tt.source)
			}
			if tt.kind == SpecInterval && got.Every != tt.duration {
				t.Fatalf("Every = %v, want %v", got.Every, tt.duration)
			}
		})
	}
}

func TestParseScheduleInvalid(t *testing.T) {
	t.Parallel()
	_, err := ParseSchedule("not-a-schedule")
	if err == nil {
		t.Fatal("expected error for invalid schedule")
	}
}

func TestParseHHMM(t *testing.T) {
	t.Parallel()
	h, m, err := parseHHMM("23:15")
	if err != nil {
		t.Fatalf("parseHHMM error: %v", err)
	}
	if h != 23 || m != 15 {
		t.Fatalf("unexpected result: %d:%d", h, m)
	}

	if _, _, err := parseHHMM("24:00"); err == nil {
		t.Fatal("expected error for invalid hour")
	}
}
