package logx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/time/rate"

	kit "pewbot/internal/transport"
)

// ---- Config ----

type Config struct {
	Level    string
	Console  bool
	File     FileConfig
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

// ---- Logger API ----

type Level = zerolog.Level

const (
	LevelTrace = zerolog.TraceLevel
	LevelDebug = zerolog.DebugLevel
	LevelInfo  = zerolog.InfoLevel
	LevelWarn  = zerolog.WarnLevel

	LevelError = zerolog.ErrorLevel
)

const consoleTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// Field mutates a zerolog event.
//
// This intentionally mirrors the ergonomics of slog.Attr without depending on slog.
// Use helpers like String(), Int(), Any(), Err(), Duration(), ...
//
// Note: Fields are applied in-order.
// If you set the same key multiple times, later fields win.
//
// The console writer will render these as key=value pairs.
// JSON sinks will keep them structured.
type Field func(e *zerolog.Event)

func String(k, v string) Field  { return func(e *zerolog.Event) { e.Str(k, v) } }
func Int(k string, v int) Field { return func(e *zerolog.Event) { e.Int(k, v) } }
func Int64(k string, v int64) Field {
	return func(e *zerolog.Event) { e.Int64(k, v) }
}
func Uint64(k string, v uint64) Field {
	return func(e *zerolog.Event) { e.Uint64(k, v) }
}
func Bool(k string, v bool) Field { return func(e *zerolog.Event) { e.Bool(k, v) } }
func Float64(k string, v float64) Field {
	return func(e *zerolog.Event) { e.Float64(k, v) }
}
func Duration(k string, v time.Duration) Field {
	return func(e *zerolog.Event) { e.Dur(k, v) }
}
func Time(k string, v time.Time) Field { return func(e *zerolog.Event) { e.Time(k, v) } }
func Any(k string, v any) Field        { return func(e *zerolog.Event) { e.Interface(k, v) } }
func Err(err error) Field {
	return func(e *zerolog.Event) {
		if err != nil {
			e.Err(err)
		}
	}
}

func Stack(stack string) Field {
	return func(e *zerolog.Event) {
		if strings.TrimSpace(stack) != "" {
			e.Str("stack", stack)
		}
	}
}

// Logger is a lightweight structured logger.
//
// - If created from Service, it stays "live" across Service.Apply() calls.
// - With() returns a derived logger with additional fixed fields.
// - Zero value is a safe no-op logger.
type Logger struct {
	svc     *Service
	base    zerolog.Logger
	hasBase bool

	fields []Field
}

// Nop returns a logger that never writes anything.
func Nop() Logger {
	return Logger{base: zerolog.Nop(), hasBase: true}
}

// NewConsole creates a standalone console logger (no Service, no fanout).
// Useful for bootstrapping components before the full log service is initialized.
func NewConsole(level string) Logger {
	// Keep timestamps short and readable.
	zerolog.TimeFieldFormat = consoleTimeFormat
	zerolog.ErrorFieldName = "err"

	cw := zerolog.ConsoleWriter{Out: Stdout(), TimeFormat: consoleTimeFormat}
	zl := zerolog.New(cw).Level(parseLevel(level, zerolog.InfoLevel)).With().Timestamp().Logger()
	return Logger{base: zl, hasBase: true}
}

func (l Logger) IsZero() bool { return l.svc == nil && !l.hasBase && len(l.fields) == 0 }

func (l Logger) root() zerolog.Logger {
	if l.svc != nil {
		return l.svc.current()
	}
	if l.hasBase {
		return l.base
	}
	return zerolog.Nop()
}

// Enabled reports whether the given level would be logged.
func (l Logger) Enabled(level Level) bool {
	zl := l.root()
	return level >= zl.GetLevel()
}

func (l Logger) With(fields ...Field) Logger {
	if len(fields) == 0 {
		return l
	}
	cp := l
	cp.fields = append(append([]Field(nil), l.fields...), fields...)
	return cp
}

func (l Logger) Trace(msg string, fields ...Field) { l.log(zerolog.TraceLevel, msg, fields...) }
func (l Logger) Debug(msg string, fields ...Field) { l.log(zerolog.DebugLevel, msg, fields...) }
func (l Logger) Info(msg string, fields ...Field)  { l.log(zerolog.InfoLevel, msg, fields...) }
func (l Logger) Warn(msg string, fields ...Field)  { l.log(zerolog.WarnLevel, msg, fields...) }
func (l Logger) Error(msg string, fields ...Field) { l.log(zerolog.ErrorLevel, msg, fields...) }

func (l Logger) log(level zerolog.Level, msg string, fields ...Field) {
	zl := l.root()
	e := zl.WithLevel(level)
	if e == nil {
		return
	}

	// Caller: keep it short (file:line), avoid noisy function names and full paths.
	if caller := shortCaller(3); caller != "" {
		e.Str(zerolog.CallerFieldName, caller)
	}

	// Fixed fields from With().
	for _, f := range l.fields {
		if f != nil {
			f(e)
		}
	}
	// Call-site fields.
	for _, f := range fields {
		if f != nil {
			f(e)
		}
	}

	e.Msg(msg)
}

func shortCaller(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok || file == "" {
		return ""
	}
	return filepath.Base(file) + ":" + strconv.Itoa(line)
}

func stackTrace(skip, maxFrames int) string {
	if maxFrames <= 0 {
		maxFrames = 16
	}
	pcs := make([]uintptr, maxFrames)
	n := runtime.Callers(skip, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	i := 0
	for {
		fr, more := frames.Next()
		if fr.File != "" {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(fr.Function)
			b.WriteString("\n  ")
			b.WriteString(fr.File)
			b.WriteString(":")
			b.WriteString(strconv.Itoa(fr.Line))
			i++
		}
		if !more || i >= maxFrames {
			break
		}
	}
	return b.String()
}

// ---- Service (dynamic config + sinks) ----

type Service struct {
	mu  sync.Mutex
	cfg Config

	root atomic.Value // stores zerolog.Logger

	file *os.File

	// telegram logging
	sender   kit.Adapter
	tgQueue  chan telegramItem
	tgOnce   sync.Once
	tgCancel context.CancelFunc
	tgWG     sync.WaitGroup

	// guarded by mu
	chatID   int64
	threadID int
	limiter  *rate.Limiter
	minLevel zerolog.Level
}

type telegramItem struct {
	to  kit.ChatTarget
	msg string
}

// New creates the logging service, applies the initial config immediately,
// and returns both the Service and a root Logger.
func New(cfg Config, sender kit.Adapter) (*Service, Logger) {
	// Global zerolog knobs.
	zerolog.ErrorFieldName = "err"
	zerolog.TimeFieldFormat = consoleTimeFormat

	s := &Service{
		cfg:      cfg,
		sender:   sender,
		tgQueue:  make(chan telegramItem, 256),
		threadID: cfg.Telegram.ThreadID,
	}

	// Safe bootstrap root.
	boot := newConsoleRoot(parseLevel(cfg.Level, zerolog.InfoLevel))
	s.root.Store(boot)

	// Apply immediately.
	s.Apply(cfg)

	return s, Logger{svc: s}
}

func (s *Service) current() zerolog.Logger {
	v := s.root.Load()
	if v == nil {
		return zerolog.Nop()
	}
	zl, ok := v.(zerolog.Logger)
	if !ok {
		return zerolog.Nop()
	}
	return zl
}

func (s *Service) Logger() Logger { return Logger{svc: s} }

func (s *Service) SetTelegramTarget(chatID int64, threadID int) {
	s.mu.Lock()
	s.chatID = chatID
	if threadID != 0 {
		s.threadID = threadID
	}
	s.mu.Unlock()
}

func (s *Service) Close() error {
	s.mu.Lock()
	f := s.file
	s.file = nil
	cancel := s.tgCancel
	s.tgCancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		s.tgWG.Wait()
	}
	if f != nil {
		_ = f.Close()
	}
	return nil
}

// Apply swaps logger outputs/levels at runtime.
// It is safe to call concurrently.
func (s *Service) Apply(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cfg = cfg

	// Update telegram knobs.
	s.minLevel = parseLevel(cfg.Telegram.MinLevel, zerolog.WarnLevel)
	rps := max(1, cfg.Telegram.RatePerSec)
	s.limiter = rate.NewLimiter(rate.Limit(rps), rps)
	if cfg.Telegram.ThreadID != 0 {
		s.threadID = cfg.Telegram.ThreadID
	}

	// Close previous file (if any).
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}

	lvl := parseLevel(cfg.Level, zerolog.InfoLevel)

	writers := make([]io.Writer, 0, 3)
	if cfg.Console {
		writers = append(writers, newConsoleWriter(Stdout()))
	}
	if cfg.File.Enabled {
		path := strings.TrimSpace(cfg.File.Path)
		if path == "" {
			path = "./pewbot.log"
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "logx: failed opening log file %q: %v\n", path, err)
		} else {
			s.file = f
			writers = append(writers, zerolog.SyncWriter(f))
		}
	}

	if cfg.Telegram.Enabled {
		// Start worker once.
		s.tgOnce.Do(func() {
			ctx, cancel := context.WithCancel(context.Background())
			s.tgCancel = cancel
			s.tgWG.Add(1)
			go func() {
				defer s.tgWG.Done()
				s.telegramWorker(ctx)
			}()
		})
		writers = append(writers, &telegramWriter{svc: s})
		if s.chatID == 0 {
			fmt.Fprintln(os.Stderr, "logx: telegram logging enabled but telegram.group_log is not set (chat_id missing)")
		}
	}

	if len(writers) == 0 {
		writers = append(writers, newConsoleWriter(Stdout()))
	}

	mw := zerolog.MultiLevelWriter(writers...)
	zl := zerolog.New(mw).Level(lvl).With().Timestamp().Logger()
	// Store as current root.
	s.root.Store(zl)
}

func newConsoleRoot(lvl zerolog.Level) zerolog.Logger {
	cw := newConsoleWriter(Stdout())
	return zerolog.New(cw).Level(lvl).With().Timestamp().Logger()
}

func newConsoleWriter(w io.Writer) io.Writer {
	cw := zerolog.ConsoleWriter{Out: w, TimeFormat: consoleTimeFormat}
	// Keep caller short and stable.
	cw.FormatCaller = func(i interface{}) string {
		s, _ := i.(string)
		if s == "" {
			return ""
		}
		return s
	}
	return cw
}

func (s *Service) telegramWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-s.tgQueue:
			if s.sender == nil {
				continue
			}
			_, _ = s.sender.SendText(ctx, it.to, it.msg, &kit.SendOptions{DisablePreview: true})
		}
	}
}

func (s *Service) enqueueTelegramLog(to kit.ChatTarget, msg string) {
	// Never block core logging.
	select {
	case s.tgQueue <- telegramItem{to: to, msg: msg}:
	default:
		// drop
	}
}

// ---- Telegram writer (zerolog sink) ----

type telegramWriter struct{ svc *Service }

func (w *telegramWriter) Write(p []byte) (int, error) {
	// Default to info when WriteLevel isn't used.
	return w.WriteLevel(zerolog.InfoLevel, p)
}

func (w *telegramWriter) WriteLevel(level zerolog.Level, p []byte) (int, error) {
	s := w.svc
	if s == nil {
		return len(p), nil
	}

	s.mu.Lock()
	chatID := s.chatID
	threadID := s.threadID
	lim := s.limiter
	min := s.minLevel
	s.mu.Unlock()

	if chatID == 0 || s.sender == nil || lim == nil {
		return len(p), nil
	}

	if level < min {
		return len(p), nil
	}
	if !lim.Allow() {
		return len(p), nil
	}

	msg := formatTelegramJSON(p)
	if msg == "" {
		return len(p), nil
	}

	to := kit.ChatTarget{ChatID: chatID, ThreadID: threadID}
	s.enqueueTelegramLog(to, msg)
	return len(p), nil
}

func formatTelegramJSON(p []byte) string {
	// Best-effort decode of a zerolog JSON line.
	var m map[string]any
	if err := json.Unmarshal(bytesTrimSpace(p), &m); err != nil {
		// Not JSON; send raw (trimmed), but cap length.
		s := strings.TrimSpace(string(p))
		return truncate(s, 3500)
	}

	lvl, _ := m["level"].(string)
	msg, _ := m["message"].(string)
	if msg == "" {
		msg, _ = m["msg"].(string)
	}

	var b strings.Builder
	if lvl != "" {
		b.WriteString("[")
		b.WriteString(strings.ToUpper(lvl))
		b.WriteString("] ")
	}
	b.WriteString(msg)

	for k, v := range m {
		if k == "time" || k == "level" || k == "message" || k == "msg" {
			continue
		}
		if k == "stack" {
			s := fmt.Sprint(v)
			s = truncate(s, 900)
			b.WriteString("\n- stack=\n")
			b.WriteString(s)
			continue
		}
		b.WriteString("\n- ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(truncate(fmt.Sprint(v), 600))
	}

	return truncate(b.String(), 3500)
}

func bytesTrimSpace(b []byte) []byte {
	i := 0
	j := len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}

func truncate(s string, maxN int) string {
	if maxN <= 0 || len(s) <= maxN {
		return s
	}
	if maxN < 10 {
		return s[:maxN]
	}
	return s[:maxN-3] + "..."
}

func parseLevel(s string, def zerolog.Level) zerolog.Level {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch s {
	case "TRACE":
		return zerolog.TraceLevel
	case "DEBUG":
		return zerolog.DebugLevel
	case "INFO":
		return zerolog.InfoLevel
	case "WARN", "WARNING":
		return zerolog.WarnLevel
	case "ERROR":
		return zerolog.ErrorLevel
	default:
		return def
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Stdout returns the configured stdout sink.
func Stdout() io.Writer { return os.Stdout }

// Stderr returns the configured stderr sink.
func Stderr() io.Writer { return os.Stderr }
