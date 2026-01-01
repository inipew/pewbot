package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
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
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (m *ConfigManager) Commit(cfg *Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
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

func (m *ConfigManager) Subscribe(buffer int) <-chan *Config {
	ch := make(chan *Config, buffer)
	m.mu.Lock()
	m.subs = append(m.subs, ch)
	m.mu.Unlock()
	return ch
}

func (m *ConfigManager) publish(cfg *Config) {
	m.mu.RLock()
	subs := append([]chan *Config{}, m.subs...)
	m.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- cfg:
		default:
			// drop if slow subscriber (hemat resource)
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
		timer = time.AfterFunc(250*time.Millisecond, func() {
			cfg, err := m.Parse()
			if err != nil || cfg == nil {
				if m.log != nil {
					m.log.Warn("config parse failed", slog.String("err", err.Error()))
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
						m.log.Warn("config rejected", slog.String("err", err.Error()))
					}
					return
				}
			}

			m.Commit(cfg)
			m.publish(cfg)
		})
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-w.Events:
			if ev.Name == filepath.Join(dir, file) {
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
					debounce()
				}
			}
		case <-w.Errors:
			// keep watching
		}
	}
}
