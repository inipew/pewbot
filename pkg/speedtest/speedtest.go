package speedtest

import (
	"context"
	"errors"
	"time"
)

type Result struct {
	Latency time.Duration
	DownBps int64
	UpBps   int64
}

func Run(ctx context.Context) (*Result, error) {
	// Starter stub: implement later with your preferred method/library.
	return nil, errors.New("speedtest not implemented in starter (stub)")
}
