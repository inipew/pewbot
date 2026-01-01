package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"pewbot/internal/kit"
)

func Stdout() io.Writer { return os.Stdout }

type Config struct {
	Level   string
	Console bool
	File    FileConfig
	Telegram TelegramConfig
}

type FileConfig struct {
	Enabled bool
	Path    string
}

type TelegramConfig struct {
	Enabled    bool
	ThreadID   int
	MinLevel   string
	RatePerSec int
}

type Service struct {
	atomicH *AtomicHandler
	logger  *slog.Logger

	sender kit.Adapter

	mu sync.Mutex

	file *os.File

	// telegram target + limiter
	chatID   int64
	threadID int
	limiter  *rate.Limiter
	minLevel slog.Level
}

func New(cfg Config, sender kit.Adapter) (*Service, *slog.Logger) {
	ah := NewAtomicHandler(slog.NewTextHandler(Stdout(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	svc := &Service{
		atomicH: ah,
		logger:  slog.New(ah),
		sender:  sender,
	}
	svc.Apply(cfg)
	return svc, svc.logger
}

func (s *Service) Logger() *slog.Logger { return s.logger }

func (s *Service) SetTelegramTarget(chatID int64, threadID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatID = chatID
	s.threadID = threadID
}

func (s *Service) Apply(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	level := parseLevel(cfg.Level, slog.LevelInfo)

	var handlers []slog.Handler
	if cfg.Console {
		handlers = append(handlers, slog.NewTextHandler(Stdout(), &slog.HandlerOptions{Level: level}))
	}

	// file handler (close old safely)
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	if cfg.File.Enabled && strings.TrimSpace(cfg.File.Path) != "" {
		f, err := os.OpenFile(cfg.File.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			s.file = f
			handlers = append(handlers, slog.NewJSONHandler(f, &slog.HandlerOptions{Level: level}))
		}
	}

	// telegram logging handler (optional)
	if cfg.Telegram.Enabled && s.sender != nil {
		s.minLevel = parseLevel(cfg.Telegram.MinLevel, slog.LevelInfo)
		rps := cfg.Telegram.RatePerSec
		if rps <= 0 {
			rps = 1
		}
		s.limiter = rate.NewLimiter(rate.Limit(rps), rps)
		s.threadID = cfg.Telegram.ThreadID
		handlers = append(handlers, &TelegramHandler{svc: s, baseLevel: level})
	}

	if len(handlers) == 0 {
		handlers = append(handlers, slog.NewTextHandler(Stdout(), &slog.HandlerOptions{Level: level}))
	}
	s.atomicH.Swap(Fanout(handlers...))
}

func parseLevel(s string, def slog.Level) slog.Level {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch s {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return def
	}
}

// ---- Atomic handler (hot swap without replacing slog.Logger) ----

type AtomicHandler struct {
	mu sync.RWMutex
	h  slog.Handler
}

func NewAtomicHandler(h slog.Handler) *AtomicHandler { return &AtomicHandler{h: h} }

func (a *AtomicHandler) Swap(h slog.Handler) {
	a.mu.Lock()
	a.h = h
	a.mu.Unlock()
}
func (a *AtomicHandler) cur() slog.Handler {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.h
}

func (a *AtomicHandler) Enabled(ctx context.Context, level slog.Level) bool { return a.cur().Enabled(ctx, level) }
func (a *AtomicHandler) Handle(ctx context.Context, r slog.Record) error     { return a.cur().Handle(ctx, r) }
func (a *AtomicHandler) WithAttrs(attrs []slog.Attr) slog.Handler            { return a.cur().WithAttrs(attrs) }
func (a *AtomicHandler) WithGroup(name string) slog.Handler                 { return a.cur().WithGroup(name) }

// ---- Fanout ----

type fanout struct{ hs []slog.Handler }

func Fanout(h ...slog.Handler) slog.Handler { return &fanout{hs: h} }

func (f *fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.hs {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}
func (f *fanout) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f.hs {
		_ = h.Handle(ctx, r)
	}
	return nil
}
func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler { return f }
func (f *fanout) WithGroup(name string) slog.Handler       { return f }

// ---- Telegram handler ----

type TelegramHandler struct {
	svc       *Service
	baseLevel slog.Level
}

func (t *TelegramHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= t.baseLevel
}

func (t *TelegramHandler) Handle(ctx context.Context, r slog.Record) error {
	t.svc.mu.Lock()
	chatID := t.svc.chatID
	threadID := t.svc.threadID
	lim := t.svc.limiter
	min := t.svc.minLevel
	t.svc.mu.Unlock()

	if chatID == 0 || t.svc.sender == nil || lim == nil {
		return nil
	}
	if r.Level < min {
		return nil
	}
	if !lim.Allow() {
		return nil
	}

	msg := fmt.Sprintf("[%s] %s", r.Level.String(), r.Message)
	r.Attrs(func(a slog.Attr) bool {
		msg += fmt.Sprintf("\n- %s=%v", a.Key, a.Value.Any())
		return true
	})

	to := kit.ChatTarget{ChatID: chatID, ThreadID: threadID}
	_, _ = t.svc.sender.SendText(ctx, to, msg, &kit.SendOptions{ParseMode: ""})
	_ = time.Now()
	return nil
}

func (t *TelegramHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return t }
func (t *TelegramHandler) WithGroup(name string) slog.Handler       { return t }
