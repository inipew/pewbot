package systemdmanager

import (
	"testing"
	"time"
)

func TestCachedEnabledRespectsTTL(t *testing.T) {
	sm := &ServiceManager{enabledCache: map[string]enabledCacheEntry{}}
	now := time.Now()
	sm.enabledCache["fresh"] = enabledCacheEntry{enabled: true, expires: now.Add(time.Minute)}
	sm.enabledCache["stale"] = enabledCacheEntry{enabled: true, expires: now.Add(-time.Minute)}

	if v, ok := sm.cachedEnabled("fresh", now); !ok || !v {
		t.Fatalf("expected cached enabled true, got %v (ok=%v)", v, ok)
	}
	if _, ok := sm.cachedEnabled("stale", now); ok {
		t.Fatalf("expected stale cache miss")
	}
}

func TestCacheCleanupAndPrune(t *testing.T) {
	sm := &ServiceManager{enabledCache: map[string]enabledCacheEntry{}}
	now := time.Now()
	sm.enabledCache["expired"] = enabledCacheEntry{enabled: true, expires: now.Add(-time.Second)}
	sm.enabledCache["keep"] = enabledCacheEntry{enabled: true, expires: now.Add(time.Minute)}

	sm.cleanupEnabledCacheLocked(now)
	if _, ok := sm.enabledCache["expired"]; ok {
		t.Fatalf("expired entry was not removed")
	}
	if _, ok := sm.enabledCache["keep"]; !ok {
		t.Fatalf("expected keep entry to remain")
	}

	for i := 0; i < maxEnabledCacheEntries+5; i++ {
		sm.enabledCache[time.Now().Add(time.Duration(i)*time.Second).String()] = enabledCacheEntry{
			enabled: true,
			expires: now.Add(time.Duration(i) * time.Second),
		}
	}
	for len(sm.enabledCache) > maxEnabledCacheEntries {
		sm.pruneEnabledCacheLocked()
	}
	if len(sm.enabledCache) != maxEnabledCacheEntries {
		t.Fatalf("cache size = %d, want %d", len(sm.enabledCache), maxEnabledCacheEntries)
	}
}
