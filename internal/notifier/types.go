package notifier

import "time"

// Config controls the async notification pipeline.
type Config struct {
	Enabled         bool
	Workers         int
	QueueSize       int
	RatePerSec      int
	RetryMax        int
	RetryBase       time.Duration
	RetryMaxDelay   time.Duration
	DedupWindow     time.Duration
	DedupMaxEntries int
	PersistDedup    bool
}

type HistoryItem struct {
	At   time.Time
	Text string
}

// NotificationEvent is emitted on the event bus for notifier lifecycle events.
// Keep it small; Data may be logged/serialized by subscribers.
type NotificationEvent struct {
	Channel  string    `json:"channel"`
	ChatID   int64     `json:"chat_id"`
	ThreadID int       `json:"thread_id,omitempty"`
	Key      string    `json:"key"`
	At       time.Time `json:"at"`
	Error    string    `json:"error,omitempty"`
}
