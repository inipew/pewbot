package tgui

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// TokenStore is an in-memory TTL store for large callback payloads.
//
// Telegram limits callback_data to 64 bytes. For rich UI flows, this store lets
// you keep large/structured payloads server-side and pass only a short token in
// callback_data.
//
// Tokens are safe for callback payloads (they never contain ':').
type TokenStore struct {
	mu sync.RWMutex

	max int
	ttl time.Duration

	// cleanupInterval controls how often we run an O(n) sweep to drop expired
	// tokens. This avoids scanning the entire map on every Get/Put.
	cleanupInterval time.Duration
	nextCleanup     time.Time

	m map[string]tokenEntry
}

type tokenEntry struct {
	b   []byte
	exp time.Time
}

// NewTokenStore creates a TokenStore with sensible defaults.
// Defaults: ttl=15m, max=5000, cleanupInterval=1m.
func NewTokenStore() *TokenStore {
	return &TokenStore{
		ttl:             15 * time.Minute,
		max:             5000,
		cleanupInterval: 1 * time.Minute,
		m:               map[string]tokenEntry{},
	}
}

// WithTTL sets the token TTL.
func (s *TokenStore) WithTTL(ttl time.Duration) *TokenStore {
	if s == nil {
		return s
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	s.mu.Lock()
	s.ttl = ttl
	// Keep cleanup reasonably frequent when TTL is small.
	if s.cleanupInterval <= 0 {
		s.cleanupInterval = 1 * time.Minute
	}
	s.mu.Unlock()
	return s
}

// WithCleanupInterval sets how often we sweep expired tokens.
// A shorter interval means less memory retention at the cost of more frequent
// O(n) sweeps. Defaults to 1 minute.
func (s *TokenStore) WithCleanupInterval(d time.Duration) *TokenStore {
	if s == nil {
		return s
	}
	if d <= 0 {
		d = 1 * time.Minute
	}
	s.mu.Lock()
	s.cleanupInterval = d
	s.mu.Unlock()
	return s
}

// WithMax sets the maximum number of live entries.
func (s *TokenStore) WithMax(max int) *TokenStore {
	if s == nil {
		return s
	}
	if max <= 0 {
		max = 5000
	}
	s.mu.Lock()
	s.max = max
	s.mu.Unlock()
	return s
}

// PutBytes stores b and returns a short token.
func (s *TokenStore) PutBytes(b []byte) string {
	if s == nil {
		return ""
	}
	if b == nil {
		b = []byte{}
	}

	// token format: "~" + base64url(6 random bytes) => 1 + 8 chars
	var buf [6]byte
	now := time.Now()

	s.maybeCleanup(now)

	// Keep lock hold time small: generate token candidates outside the lock,
	// then lock briefly to check/insert.
	for i := 0; i < 8; i++ {
		_, _ = rand.Read(buf[:])
		tok := "~" + base64.RawURLEncoding.EncodeToString(buf[:])

		s.mu.Lock()
		if _, exists := s.m[tok]; exists {
			s.mu.Unlock()
			continue
		}
		s.m[tok] = tokenEntry{b: append([]byte(nil), b...), exp: now.Add(s.ttl)}
		s.enforceMaxLocked()
		s.mu.Unlock()
		return tok
	}

	// Extremely unlikely collision fallback: include a time byte.
	_, _ = rand.Read(buf[:])
	tok := "~" + base64.RawURLEncoding.EncodeToString(append(buf[:], byte(now.UnixNano())))
	s.mu.Lock()
	s.m[tok] = tokenEntry{b: append([]byte(nil), b...), exp: now.Add(s.ttl)}
	s.enforceMaxLocked()
	s.mu.Unlock()
	return tok
}

// PutString stores a string and returns a token.
func (s *TokenStore) PutString(v string) string {
	return s.PutBytes([]byte(v))
}

// PutJSON stores JSON-marshaled v and returns a token.
func (s *TokenStore) PutJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return s.PutBytes(b), nil
}

// GetBytes returns stored bytes for tok.
func (s *TokenStore) GetBytes(tok string) ([]byte, bool) {
	if s == nil || tok == "" {
		return nil, false
	}

	now := time.Now()
	s.maybeCleanup(now)

	// Fast path: read lock.
	s.mu.RLock()
	e, ok := s.m[tok]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if !e.exp.IsZero() && now.After(e.exp) {
		// Slow path: delete expired entry.
		s.mu.Lock()
		// Re-check under write lock.
		e2, ok2 := s.m[tok]
		if ok2 && !e2.exp.IsZero() && now.After(e2.exp) {
			delete(s.m, tok)
		}
		s.mu.Unlock()
		return nil, false
	}
	return append([]byte(nil), e.b...), true
}

// GetString returns stored string for tok.
func (s *TokenStore) GetString(tok string) (string, bool) {
	b, ok := s.GetBytes(tok)
	return string(b), ok
}

// GetJSON unmarshals stored bytes into out.
func (s *TokenStore) GetJSON(tok string, out any) error {
	b, ok := s.GetBytes(tok)
	if !ok {
		return errors.New("tgui: token not found")
	}
	return json.Unmarshal(b, out)
}

func (s *TokenStore) cleanupLocked(now time.Time) {
	for k, e := range s.m {
		if !e.exp.IsZero() && now.After(e.exp) {
			delete(s.m, k)
		}
	}
}

func (s *TokenStore) maybeCleanup(now time.Time) {
	if s == nil {
		return
	}
	s.mu.RLock()
	interval := s.cleanupInterval
	next := s.nextCleanup
	s.mu.RUnlock()

	if interval <= 0 {
		interval = 1 * time.Minute
	}
	if next.IsZero() {
		// Initialize lazily.
		s.mu.Lock()
		if s.nextCleanup.IsZero() {
			s.nextCleanup = now.Add(interval)
		}
		s.mu.Unlock()
		return
	}
	if now.Before(next) {
		return
	}

	s.mu.Lock()
	// Re-check under lock.
	if s.nextCleanup.IsZero() || !now.Before(s.nextCleanup) {
		s.cleanupLocked(now)
		s.nextCleanup = now.Add(interval)
	}
	s.mu.Unlock()
}

func (s *TokenStore) enforceMaxLocked() {
	if s.max <= 0 {
		return
	}
	if len(s.m) <= s.max {
		return
	}
	// Best-effort eviction: remove arbitrary entries until within limit.
	over := len(s.m) - s.max
	for k := range s.m {
		delete(s.m, k)
		over--
		if over <= 0 {
			break
		}
	}
}
