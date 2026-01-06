package pprof

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	hpprof "net/http/pprof"
	logx "pewbot/pkg/logx"
	"runtime"
	"strings"
	"sync"
	"time"

	rtsup "pewbot/internal/runtime/supervisor"
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
	log logx.Logger
	cfg Config

	ln       net.Listener
	srv      *http.Server
	sup      *rtsup.Supervisor
	stopDone chan struct{}
}

// Supervisor returns the pprof service's internal supervisor (nil if not started).
// This is used for operational visibility (e.g. /health).
func (s *Service) Supervisor() *rtsup.Supervisor {
	s.mu.Lock()
	sup := s.sup
	s.mu.Unlock()
	return sup
}

func New(cfg Config, log logx.Logger) *Service {
	if log.IsZero() {
		log = logx.Nop()
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
	if ctx == nil {
		ctx = context.Background()
	}

	// Start is idempotent.
	for {
		s.mu.Lock()
		// If stopping, wait for it to finish before restarting.
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
		if s.sup != nil {
			s.mu.Unlock()
			return
		}
		cur := s.cfg
		if !cur.Enabled {
			s.mu.Unlock()
			return
		}

		s.sup = rtsup.NewSupervisor(ctx,
			rtsup.WithLogger(s.log.With(logx.String("comp", "pprof"))),
			// pprof is optional observability; never hard-kill the app.
			rtsup.WithCancelOnError(false),
		)
		sup := s.sup
		s.mu.Unlock()

		// Run the HTTP server under a restart loop so pprof self-heals.
		sup.GoRestart("http.serve", func(c context.Context) error {
			return s.serveOnce(c)
		},
			rtsup.WithPublishFirstError(true),
			rtsup.WithRestartBackoff(500*time.Millisecond, 10*time.Second),
		)
		return
	}
}

func (s *Service) Stop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	// If not running, nothing to do.
	if s.sup == nil {
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
	sup := s.sup
	s.mu.Unlock()

	// Shutdown happens asynchronously so callers can time out without leaking state.
	go func() {
		defer close(done)

		// Best-effort graceful shutdown.
		if srv != nil {
			shutdownCtx := ctx
			if shutdownCtx == nil {
				shutdownCtx = context.Background()
			}
			_ = srv.Shutdown(shutdownCtx)
			_ = srv.Close()
		}
		if ln != nil {
			_ = ln.Close()
		}
		if sup != nil {
			sup.Cancel()
			_ = sup.Wait(context.Background())
		}

		s.mu.Lock()
		s.ln = nil
		s.srv = nil
		s.sup = nil
		s.stopDone = nil
		s.mu.Unlock()
		s.log.Info("pprof stopped")
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
		// Force-stop internal loops.
		if sup != nil {
			sup.Cancel()
		}
		return
	}
}

func (s *Service) serveOnce(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	cur := s.cfg
	log := s.log
	s.mu.Unlock()

	if !cur.Enabled {
		return context.Canceled
	}

	addr := strings.TrimSpace(cur.Addr)
	if addr == "" {
		addr = "127.0.0.1:6060"
	}

	// Safety: prevent accidental public exposure without auth.
	if !cur.AllowInsecure && cur.Token == "" && !isLoopbackAddr(addr) {
		log.Error("pprof refused to start: non-loopback addr requires token or allow_insecure",
			logx.String("addr", addr),
		)
		return errors.New("pprof refused to start: insecure bind")
	}
	if cur.AllowInsecure && cur.Token == "" && !isLoopbackAddr(addr) {
		log.Warn("pprof running without token on non-loopback addr (insecure)", logx.String("addr", addr))
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("pprof listen failed", logx.String("addr", addr), logx.Any("err", err))
		if ctx.Err() != nil {
			return context.Canceled
		}
		return err
	}
	defer func() { _ = ln.Close() }()

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
	defer func() { _ = srv.Close() }()

	// Expose server handles for Stop().
	s.mu.Lock()
	s.ln = ln
	s.srv = srv
	s.mu.Unlock()

	// Ensure the server is stopped when the supervisor context is cancelled.
	go func() {
		<-ctx.Done()
		// Keep this bounded; the outer Stop(ctx) does the real graceful shutdown.
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(cctx)
		cancel()
	}()

	listenAddr := ln.Addr().String()
	urlHint := fmt.Sprintf("http://%s%s", listenAddr, prefix)
	log.Info("pprof started", logx.String("addr", listenAddr), logx.String("prefix", prefix), logx.Bool("token_set", cur.Token != ""), logx.String("hint", urlHint))

	err = srv.Serve(ln)

	// Clear handles if we still own them.
	s.mu.Lock()
	if s.srv == srv {
		s.srv = nil
		s.ln = nil
	}
	stopping := s.stopDone != nil
	s.mu.Unlock()

	if stopping || ctx.Err() != nil {
		return context.Canceled
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return errors.New("pprof server exited unexpectedly")
	}
	return err
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
