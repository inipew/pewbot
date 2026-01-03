package core

import (
	"context"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type ConfigManager struct {
	path string

	mu   sync.RWMutex
	cfg  *Config
	subs []chan *Config

	log       *slog.Logger
	validator func(ctx context.Context, cfg *Config) error

	// lastHash tracks the last successfully committed config content.
	// It helps avoid redundant publishes when the editor causes multiple write events
	// without content changes.
	lastHash uint64
}

func NewConfigManager(path string) *ConfigManager {
	return &ConfigManager{path: path}
}

func (m *ConfigManager) SetLogger(log *slog.Logger) { m.log = log }

// SetValidator installs a validation hook used by Watch() before committing/publishing.
func (m *ConfigManager) SetValidator(fn func(ctx context.Context, cfg *Config) error) {
	m.validator = fn
}

func (m *ConfigManager) Parse() (*Config, error) {
	b, err := os.ReadFile(m.path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(b))
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
	m.mu.Lock()
	m.subs = append(m.subs, ch)
	m.mu.Unlock()
	return ch
}

func (m *ConfigManager) Unsubscribe(ch chan *Config) {
	if ch == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range m.subs {
		if s == ch {
			// swap-remove (order doesn't matter)
			last := len(m.subs) - 1
			m.subs[i] = m.subs[last]
			m.subs[last] = nil
			m.subs = m.subs[:last]
			return
		}
	}
}

func (m *ConfigManager) publish(cfg *Config) {
	m.mu.RLock()
	subs := append([]chan *Config{}, m.subs...)
	m.mu.RUnlock()
	for _, ch := range subs {
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
				if m.log != nil {
					m.log.Debug(
						"config update dropped (subscriber slow)",
						slog.Int("queue_len", len(ch)),
						slog.Int("queue_cap", cap(ch)),
					)
				}
			}
		}
	}
}

func (m *ConfigManager) Watch(ctx context.Context) error {
	dir := filepath.Dir(m.path)
	file := filepath.Base(m.path)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	if err := w.Add(dir); err != nil {
		return err
	}

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
		if m.log != nil {
			m.log.Debug("config change detected; scheduling reload", slog.String("path", m.path))
		}
		timer = time.AfterFunc(250*time.Millisecond, func() {
			cfg, err := m.Parse()
			if err != nil || cfg == nil {
				if m.log != nil {
					errStr := "<nil>"
					if err != nil {
						errStr = err.Error()
					} else if cfg == nil {
						errStr = "config is nil"
					}
					m.log.Warn("config parse failed", slog.String("path", m.path), slog.String("err", errStr))
				}
				return
			}

			// Skip redundant reloads when content is unchanged.
			h := hashConfig(cfg)
			m.mu.RLock()
			unchanged := h != 0 && h == m.lastHash
			m.mu.RUnlock()
			if unchanged {
				if m.log != nil {
					m.log.Debug("config unchanged; skipping publish", slog.String("path", m.path))
				}
				return
			}

			// validate before commit/publish (transactional)
			if m.validator != nil {
				vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := m.validator(vctx, cfg)
				cancel()
				if err != nil {
					if m.log != nil {
						m.log.Warn("config rejected", slog.String("path", m.path), slog.Any("err", err))
					}
					return
				}
			}

			m.Commit(cfg)
			m.publish(cfg)
			if m.log != nil {
				m.log.Debug("config published", slog.String("path", m.path), slog.String("hash", fmt.Sprintf("%x", h)))
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			// Compare by basename (more robust across absolute/relative paths and OS quirks).
			if strings.EqualFold(filepath.Base(ev.Name), file) {
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove|fsnotify.Chmod) != 0 {
					debounce()
				}
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			if m.log != nil && err != nil {
				m.log.Warn("config watch error", slog.Any("err", err), slog.String("dir", dir))
			}
		}
	}
}
