package core

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

type HandlerFunc func(ctx context.Context, req *Request) error

type Middleware func(next HandlerFunc) HandlerFunc

func Chain(h HandlerFunc, m ...Middleware) HandlerFunc {
	for i := len(m) - 1; i >= 0; i-- {
		h = m[i](h)
	}
	return h
}

func MWTimeout(d time.Duration) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, req *Request) error {
			if d <= 0 {
				return next(ctx, req)
			}
			cctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(cctx, req)
		}
	}
}

func MWPanicRecover(log *slog.Logger) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, req *Request) (err error) {
			defer func() {
				if r := recover(); r != nil {
					logger := log
					if req != nil && req.Logger != nil {
						logger = req.Logger
					}
					logger.Error("panic recovered",
						slog.Any("panic", r),
						slog.String("stack", string(debug.Stack())),
					)
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return next(ctx, req)
		}
	}
}

func MWRequestLog(log *slog.Logger) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, req *Request) error {
			start := time.Now()
			logger := log
			if req != nil && req.Logger != nil {
				logger = req.Logger
			}
			err := next(ctx, req)
			d := time.Since(start)

			fields := []any{
				slog.String("kind", string(req.Update.Kind)),
				slog.Int64("chat_id", req.Chat.ChatID),
				slog.Int("thread_id", req.Chat.ThreadID),
				slog.Int64("from_id", req.FromID),
				slog.String("cmd", req.Command),
				slog.Duration("dur", d),
			}
			if err != nil {
				logger.Warn("request failed", append(fields, slog.String("err", err.Error()))...)
			} else {
				logger.Info("request ok", fields...)
			}
			return err
		}
	}
}
