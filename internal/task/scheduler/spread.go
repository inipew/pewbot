package scheduler

import (
	"hash/fnv"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
)

const maxStartupSpread = 30 * time.Second

// startupSpreadSchedule wraps a base schedule and overrides the first run time.
// After the first run, it delegates to the base schedule.
type startupSpreadSchedule struct {
	base  cron.Schedule
	first time.Time
}

func (s *startupSpreadSchedule) Next(t time.Time) time.Time {
	if !s.first.IsZero() && t.Before(s.first) {
		return s.first
	}
	return s.base.Next(t)
}

var spreadSeq uint64

func makeIntervalScheduleWithSpread(every time.Duration, now time.Time, tag string) (cron.Schedule, time.Duration) {
	base := cron.Every(every)
	spreadMax := every
	if spreadMax > maxStartupSpread {
		spreadMax = maxStartupSpread
	}
	if spreadMax <= 0 {
		return base, 0
	}

	seed := time.Now().UnixNano() ^ int64(atomic.AddUint64(&spreadSeq, 1)) ^ int64(fnv64a(tag))
	rng := rand.New(rand.NewSource(seed))
	jitter := time.Duration(rng.Int63n(int64(spreadMax)))
	first := now.Add(every + jitter)
	return &startupSpreadSchedule{base: base, first: first}, jitter
}

func fnv64a(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
