package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	logx "pewbot/pkg/logx"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type ConfigManager struct {
	path string

	mu  sync.RWMutex
	cfg *Config

	// subsMu guards subscriber list and ensures we never send on a channel
	// that is concurrently being closed in Unsubscribe().
	subsMu sync.Mutex
	subs   []chan *Config

	log       logx.Logger
	validator func(ctx context.Context, cfg *Config) error

	// lastHash tracks the last successfully committed config content.
	// It helps avoid redundant publishes when the editor causes multiple write events
	// without content changes.
	lastHash uint64
}

func NewConfigManager(path string) *ConfigManager {
	return &ConfigManager{path: path}
}

func (m *ConfigManager) SetLogger(log logx.Logger) { m.log = log }

// SetValidator installs a validation hook used by Watch() before committing/publishing.
func (m *ConfigManager) SetValidator(fn func(ctx context.Context, cfg *Config) error) {
	m.validator = fn
}

func (m *ConfigManager) Parse() (*Config, error) {
	b, err := os.ReadFile(m.path)
	if err != nil {
		return nil, err
	}
	jb, _, err := coerceToJSONBytes(m.path, b)
	if err != nil {
		return nil, err
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(jb))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	// reject trailing tokens (e.g. concatenated JSON)
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("invalid config: trailing data")
		}
		return nil, err
	}
	return &cfg, nil
}

func (m *ConfigManager) Commit(cfg *Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.lastHash = hashConfig(cfg)
	m.mu.Unlock()
}

func hashConfig(cfg *Config) uint64 {
	if cfg == nil {
		return 0
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return 0
	}
	return hashBytes(b)
}

func (m *ConfigManager) Load() (*Config, error) {
	cfg, err := m.Parse()
	if err != nil {
		return nil, err
	}
	m.Commit(cfg)
	return cfg, nil
}

func (m *ConfigManager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *ConfigManager) Subscribe(buffer int) chan *Config {
	ch := make(chan *Config, buffer)
	m.subsMu.Lock()
	m.subs = append(m.subs, ch)
	m.subsMu.Unlock()
	return ch
}

func (m *ConfigManager) Unsubscribe(ch chan *Config) {
	if ch == nil {
		return
	}
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for i, s := range m.subs {
		if s == ch {
			// swap-remove (order doesn't matter)
			last := len(m.subs) - 1
			m.subs[i] = m.subs[last]
			m.subs[last] = nil
			m.subs = m.subs[:last]
			close(ch)
			return
		}
	}
}

func (m *ConfigManager) publish(cfg *Config) {
	// Hold subsMu while sending to avoid send-on-closed panics.
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for _, ch := range m.subs {
		if ch == nil {
			continue
		}
		// Always try to deliver the latest config.
		// If subscriber is slow and buffer is full, drop ONE oldest item then push the newest.
		select {
		case ch <- cfg:
			// delivered
		default:
			// drop oldest (if any)
			select {
			case <-ch:
			default:
			}
			// best-effort deliver latest
			select {
			case ch <- cfg:
			default:
				// still full; give up
				if !m.log.IsZero() {
					m.log.Debug(
						"config update dropped (subscriber slow)",
						logx.Int("queue_len", len(ch)),
						logx.Int("queue_cap", cap(ch)),
					)
				}
			}
		}
	}
}

func (m *ConfigManager) Watch(ctx context.Context) error {
	dir := filepath.Dir(m.path)
	file := filepath.Base(m.path)

	// When fsnotify gets into a bad state (common on Windows + certain editors),
	// the watcher may stop delivering events or close its channels.
	// Self-heal by recreating the watcher with a small exponential backoff.
	const (
		restartBackoffBase = 250 * time.Millisecond
		restartBackoffMax  = 5 * time.Second
	)
	backoff := restartBackoffBase
	// local RNG to avoid global contention (and to keep jitter deterministic per process).
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// debounce to avoid partial writes
	var (
		timerMu sync.Mutex
		timer   *time.Timer
	)
	debounce := func() {
		timerMu.Lock()
		defer timerMu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		if !m.log.IsZero() {
			m.log.Debug("config change detected; scheduling reload", logx.String("path", m.path))
		}
		timer = time.AfterFunc(250*time.Millisecond, func() {
			cfg, err := m.Parse()
			if err != nil || cfg == nil {
				if !m.log.IsZero() {
					errStr := "<nil>"
					if err != nil {
						errStr = err.Error()
					} else if cfg == nil {
						errStr = "config is nil"
					}
					m.log.Warn("config parse failed", logx.String("path", m.path), logx.String("err", errStr))
				}
				return
			}

			// Skip redundant reloads when content is unchanged.
			h := hashConfig(cfg)
			m.mu.RLock()
			unchanged := h != 0 && h == m.lastHash
			m.mu.RUnlock()
			if unchanged {
				if !m.log.IsZero() {
					m.log.Debug("config unchanged; skipping publish", logx.String("path", m.path))
				}
				return
			}

			// validate before commit/publish (transactional)
			if m.validator != nil {
				vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := m.validator(vctx, cfg)
				cancel()
				if err != nil {
					if !m.log.IsZero() {
						m.log.Warn("config rejected", logx.String("path", m.path), logx.Any("err", err))
					}
					return
				}
			}

			m.Commit(cfg)
			m.publish(cfg)
			if !m.log.IsZero() {
				m.log.Debug("config published", logx.String("path", m.path), logx.String("hash", fmt.Sprintf("%x", h)))
			}
		})
	}

	for {
		if ctx.Err() != nil {
			return nil
		}

		w, err := fsnotify.NewWatcher()
		if err != nil {
			if !m.log.IsZero() {
				m.log.Warn("config watch init failed", logx.Any("err", err), logx.String("dir", dir))
			}
			// retry with backoff
			wait := backoff + time.Duration(rng.Int63n(int64(backoff/2)+1))
			if backoff < restartBackoffMax {
				backoff *= 2
				if backoff > restartBackoffMax {
					backoff = restartBackoffMax
				}
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
				continue
			}
		}

		if err := w.Add(dir); err != nil {
			_ = w.Close()
			if !m.log.IsZero() {
				m.log.Warn("config watch add failed", logx.Any("err", err), logx.String("dir", dir))
			}
			wait := backoff + time.Duration(rng.Int63n(int64(backoff/2)+1))
			if backoff < restartBackoffMax {
				backoff *= 2
				if backoff > restartBackoffMax {
					backoff = restartBackoffMax
				}
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
				continue
			}
		}

		// success; reset backoff so transient issues don't cause long restart delays
		backoff = restartBackoffBase
		if !m.log.IsZero() {
			m.log.Debug("config watcher started", logx.String("dir", dir), logx.String("file", file))
		}

		// inner loop: runs until watcher breaks, then outer loop recreates it.
		broken := false
		for !broken {
			select {
			case <-ctx.Done():
				_ = w.Close()
				return nil
			case ev, ok := <-w.Events:
				if !ok {
					broken = true
					break
				}
				// Compare by basename (more robust across absolute/relative paths and OS quirks).
				if strings.EqualFold(filepath.Base(ev.Name), file) {
					if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove|fsnotify.Chmod) != 0 {
						debounce()
					}
				}
			case err, ok := <-w.Errors:
				if !ok {
					broken = true
					break
				}
				if err == nil {
					continue
				}
				// Overflow means we may have missed events; reload once and keep going.
				// Avoid depending on a specific fsnotify error constant across versions.
				if strings.Contains(strings.ToLower(err.Error()), "overflow") {
					if !m.log.IsZero() {
						m.log.Warn("config watch overflow; forcing reload", logx.Any("err", err), logx.String("dir", dir))
					}
					debounce()
					continue
				}
				if !m.log.IsZero() {
					m.log.Warn("config watch error", logx.Any("err", err), logx.String("dir", dir))
				}
				// Some fsnotify backends surface watcher closure via an error.
				if strings.Contains(strings.ToLower(err.Error()), "closed") {
					broken = true
					break
				}
			}
		}

		_ = w.Close()
		if ctx.Err() != nil {
			return nil
		}
		// restart with a small jittered backoff to avoid tight restart loops.
		wait := backoff + time.Duration(rng.Int63n(int64(backoff/2)+1))
		if !m.log.IsZero() {
			m.log.Warn(
				"config watcher stopped; restarting",
				logx.String("dir", dir),
				logx.String("file", file),
				logx.Duration("backoff", wait),
			)
		}
		if backoff < restartBackoffMax {
			backoff *= 2
			if backoff > restartBackoffMax {
				backoff = restartBackoffMax
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
			continue
		}
	}
}
