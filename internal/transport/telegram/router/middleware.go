package router

import (
	"context"
	"fmt"
	logx "pewbot/pkg/logx"
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

func MWPanicRecover(log logx.Logger) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, req *Request) (err error) {
			defer func() {
				if r := recover(); r != nil {
					logger := log
					if req != nil && !req.Logger.IsZero() {
						logger = req.Logger
					}
					logger.Error("panic recovered",
						logx.Any("panic", r),
						logx.String("stack", string(debug.Stack())),
					)
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return next(ctx, req)
		}
	}
}

func MWRequestLog(log logx.Logger) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, req *Request) error {
			start := time.Now()
			logger := log
			if req != nil && !req.Logger.IsZero() {
				logger = req.Logger
			}
			err := next(ctx, req)
			d := time.Since(start)

			fields := []logx.Field{
				logx.String("kind", string(req.Update.Kind)),
				logx.Int64("chat_id", req.Chat.ChatID),
				logx.Int("thread_id", req.Chat.ThreadID),
				logx.Int64("from_id", req.FromID),
				logx.String("cmd", req.Command),
				logx.Duration("dur", d),
			}
			if err != nil {
				logger.Warn("request failed", append(fields, logx.Any("err", err))...)
			} else {
				// Keep INFO useful: short successful requests go to DEBUG.
				if d >= 750*time.Millisecond {
					logger.Info("request ok", fields...)
				} else {
					logger.Debug("request ok", fields...)
				}
			}
			return err
		}
	}
}
