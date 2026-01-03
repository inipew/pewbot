package pprof

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	hpprof "net/http/pprof"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Config controls the optional pprof HTTP server.
//
// Security:
//   - Prefer binding to localhost (default).
//   - If binding to a non-loopback address, set Token or enable AllowInsecure.
type Config struct {
	Enabled       bool
	Addr          string
	Prefix        string
	Token         string
	AllowInsecure bool

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	MutexProfileFraction int
	BlockProfileRate     int
	MemProfileRate       int
}

type Service struct {
	mu  sync.Mutex
	log *slog.Logger
	cfg Config

	ln       net.Listener
	srv      *http.Server
	cancel   context.CancelFunc
	stopDone chan struct{}
}

func New(cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg, log: log}
}

func (s *Service) Enabled() bool {
	s.mu.Lock()
	en := s.cfg.Enabled
	s.mu.Unlock()
	return en
}

// Reconfigure applies cfg and starts/stops/restarts the server if needed.
// Safe to call during hot-reload.
func (s *Service) Reconfigure(ctx context.Context, cfg Config) {
	// Always apply runtime profiling rates even when server is disabled.
	applyRuntimeRates(cfg)

	s.mu.Lock()
	prev := s.cfg
	running := s.srv != nil
	s.cfg = cfg
	s.mu.Unlock()

	if !cfg.Enabled {
		if running {
			s.Stop(ctx)
		}
		return
	}

	if !running {
		s.Start(ctx)
		return
	}

	if needsRestart(prev, cfg) {
		s.Stop(ctx)
		s.Start(ctx)
	}
}

func needsRestart(a, b Config) bool {
	if a.Addr != b.Addr {
		return true
	}
	if normalizePrefix(a.Prefix) != normalizePrefix(b.Prefix) {
		return true
	}
	if a.Token != b.Token {
		return true
	}
	if a.AllowInsecure != b.AllowInsecure {
		return true
	}
	// Timeouts affect server behavior; easiest is restart.
	if a.ReadTimeout != b.ReadTimeout || a.WriteTimeout != b.WriteTimeout || a.IdleTimeout != b.IdleTimeout {
		return true
	}
	return false
}

func applyRuntimeRates(cfg Config) {
	// 0 keeps Go default; explicit -1 is not supported here.
	if cfg.MutexProfileFraction >= 0 {
		runtime.SetMutexProfileFraction(cfg.MutexProfileFraction)
	}
	if cfg.BlockProfileRate >= 0 {
		runtime.SetBlockProfileRate(cfg.BlockProfileRate)
	}
	if cfg.MemProfileRate > 0 {
		runtime.MemProfileRate = cfg.MemProfileRate
	}
}

func (s *Service) Start(ctx context.Context) {
	for {
		s.mu.Lock()
		// If already running, do nothing.
		if s.srv != nil {
			s.mu.Unlock()
			return
		}
		// If stop is in progress, wait for it (avoid double listen).
		if s.stopDone != nil {
			done := s.stopDone
			s.mu.Unlock()
			select {
			case <-done:
				// loop
			case <-ctx.Done():
				return
			}
			continue
		}
		cur := s.cfg
		s.mu.Unlock()

		if !cur.Enabled {
			return
		}

		addr := strings.TrimSpace(cur.Addr)
		if addr == "" {
			addr = "127.0.0.1:6060"
		}

		// Safety: prevent accidental public exposure without auth.
		if !cur.AllowInsecure && cur.Token == "" && !isLoopbackAddr(addr) {
			s.log.Error("pprof refused to start: non-loopback addr requires token or allow_insecure",
				slog.String("addr", addr),
			)
			return
		}
		if cur.AllowInsecure && cur.Token == "" && !isLoopbackAddr(addr) {
			s.log.Warn("pprof running without token on non-loopback addr (insecure)", slog.String("addr", addr))
		}

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			s.log.Error("pprof listen failed", slog.String("addr", addr), slog.Any("err", err))
			return
		}

		prefix := normalizePrefix(cur.Prefix)
		mux := http.NewServeMux()

		wrap := func(h http.HandlerFunc) http.HandlerFunc { return s.withAuth(cur.Token, h) }

		// Optional lightweight liveness endpoint.
		mux.HandleFunc("/healthz", wrap(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}))

		// pprof endpoints under prefix.
		base := strings.TrimSuffix(prefix, "/")
		mux.HandleFunc(prefix, wrap(pprofIndexAt(prefix)))
		mux.HandleFunc(base+"/cmdline", wrap(hpprof.Cmdline))
		mux.HandleFunc(base+"/profile", wrap(hpprof.Profile))
		mux.HandleFunc(base+"/symbol", wrap(hpprof.Symbol))
		mux.HandleFunc(base+"/trace", wrap(hpprof.Trace))

		// Also allow "prefix/" and "prefix" forms.
		mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
			// Redirect to canonical trailing slash.
			http.Redirect(w, r, prefix, http.StatusPermanentRedirect)
		})

		srv := &http.Server{
			Handler:      mux,
			ReadTimeout:  cur.ReadTimeout,
			WriteTimeout: cur.WriteTimeout,
			IdleTimeout:  cur.IdleTimeout,
		}

		_, cancel := context.WithCancel(ctx)

		s.mu.Lock()
		s.ln = ln
		s.srv = srv
		s.cancel = cancel
		s.mu.Unlock()

		go func() {
			err := srv.Serve(ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("pprof server stopped with error", slog.Any("err", err))
			}
		}()

		listenAddr := ln.Addr().String()
		urlHint := fmt.Sprintf("http://%s%s", listenAddr, prefix)
		s.log.Info("pprof started", slog.String("addr", listenAddr), slog.String("prefix", prefix), slog.Bool("token_set", cur.Token != ""), slog.String("hint", urlHint))
		return
	}
}

func (s *Service) Stop(ctx context.Context) {
	s.mu.Lock()
	if s.srv == nil {
		s.mu.Unlock()
		return
	}
	// If a stop is already in progress, wait for it.
	if s.stopDone != nil {
		done := s.stopDone
		s.mu.Unlock()
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}

	done := make(chan struct{})
	s.stopDone = done
	srv := s.srv
	ln := s.ln
	cancel := s.cancel
	s.srv = nil
	s.ln = nil
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// Ensure listener is closed even if Shutdown is stuck.
	if ln != nil {
		_ = ln.Close()
	}

	go func() {
		defer close(done)
		shutdownCtx := ctx
		if shutdownCtx == nil {
			shutdownCtx = context.Background()
		}
		_ = srv.Shutdown(shutdownCtx)
		_ = srv.Close()
		s.mu.Lock()
		s.stopDone = nil
		s.mu.Unlock()
		s.log.Info("pprof stopped")
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
		return
	}
}

func (s *Service) withAuth(token string, h http.HandlerFunc) http.HandlerFunc {
	tok := strings.TrimSpace(token)
	if tok == "" {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Accept either:
		//   Authorization: Bearer <token>
		// or query param: ?token=<token>
		if got := r.URL.Query().Get("token"); got != "" {
			if got == tok {
				h(w, r)
				return
			}
			unauthorized(w)
			return
		}
		if ah := r.Header.Get("Authorization"); ah != "" {
			const p = "Bearer "
			if strings.HasPrefix(ah, p) && strings.TrimSpace(strings.TrimPrefix(ah, p)) == tok {
				h(w, r)
				return
			}
		}
		unauthorized(w)
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func normalizePrefix(prefix string) string {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = "/debug/pprof/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// pprof.Index assumes requests are rooted at /debug/pprof/.
// To support custom prefixes without forking net/http/pprof,
// we rewrite the path before calling the handler.
func pprofIndexAt(prefix string) http.HandlerFunc {
	canon := normalizePrefix(prefix)
	return func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.Path, canon)
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/debug/pprof/" + suffix
		hpprof.Index(w, r2)
	}
}

func isLoopbackAddr(addr string) bool {
	// addr is expected in host:port (host may be empty).
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	h = strings.TrimSpace(h)
	if h == "" {
		// empty host means all interfaces
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
