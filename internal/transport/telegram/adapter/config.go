package adapter

import "time"

type Config struct {
	Token       string
	PollTimeout time.Duration
}
