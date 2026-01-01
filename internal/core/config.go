package core

import (
	"context"
	"encoding/json"
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
}

func NewConfigManager(path string) *ConfigManager {
	return &ConfigManager{path: path}
}

func (m *ConfigManager) Load() (*Config, error) {
	b, err := os.ReadFile(m.path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cfg = &cfg
	m.mu.Unlock()
	return &cfg, nil
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
			cfg, err := m.Load()
			if err == nil && cfg != nil {
				m.publish(cfg)
			}
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
