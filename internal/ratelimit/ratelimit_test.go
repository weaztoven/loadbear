package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestAllowDenyBurstAndRefill(t *testing.T) {
	limiter := New(2, 2, time.Minute)
	if allowed, retry := limiter.Allow("192.0.2.1"); !allowed || retry != 0 {
		t.Fatalf("first request = %v, %d", allowed, retry)
	}
	if allowed, _ := limiter.Allow("192.0.2.1"); !allowed {
		t.Fatal("second burst token was denied")
	}
	if allowed, retry := limiter.Allow("192.0.2.1"); allowed || retry != 60 {
		t.Fatalf("exhausted bucket = %v, retry %d", allowed, retry)
	}
	limiter.mu.Lock()
	limiter.buckets["192.0.2.1"].mu.Lock()
	limiter.buckets["192.0.2.1"].last = time.Now().Add(-time.Second)
	limiter.buckets["192.0.2.1"].mu.Unlock()
	limiter.mu.Unlock()
	if allowed, _ := limiter.Allow("192.0.2.1"); !allowed {
		t.Fatal("refilled token was denied")
	}
}

func TestAllowNormalizesEmptyIP(t *testing.T) {
	limiter := New(1, 1, time.Minute)
	if allowed, _ := limiter.Allow(""); !allowed {
		t.Fatal("first empty-IP request should be allowed")
	}
	if allowed, _ := limiter.Allow("unknown"); allowed {
		t.Fatal("empty IP did not share the unknown bucket")
	}
}

func TestEvictExpiredEntries(t *testing.T) {
	limiter := New(1, 1, time.Minute)
	limiter.Allow("active")
	limiter.Allow("stale")
	limiter.mu.Lock()
	limiter.buckets["stale"].mu.Lock()
	limiter.buckets["stale"].last = time.Now().Add(-2 * time.Minute)
	limiter.buckets["stale"].mu.Unlock()
	limiter.mu.Unlock()
	limiter.EvictExpired()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if _, ok := limiter.buckets["stale"]; ok {
		t.Fatal("stale bucket was not evicted")
	}
	if _, ok := limiter.buckets["active"]; !ok {
		t.Fatal("active bucket was evicted")
	}
}

func TestBackgroundEvictionStopsCleanly(t *testing.T) {
	limiter := New(1, 1, 20*time.Millisecond)
	limiter.Allow("idle-client")
	limiter.mu.Lock()
	limiter.buckets["idle-client"].mu.Lock()
	limiter.buckets["idle-client"].last = time.Now().Add(-time.Second)
	limiter.buckets["idle-client"].mu.Unlock()
	limiter.mu.Unlock()
	if err := limiter.Start(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		limiter.mu.Lock()
		_, exists := limiter.buckets["idle-client"]
		limiter.mu.Unlock()
		if !exists {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("background evictor did not remove the stale bucket")
		case <-ticker.C:
		}
	}
	limiter.Stop()
	select {
	case <-limiter.done:
	default:
		t.Fatal("eviction goroutine did not stop")
	}
	limiter.Stop()
}

func TestConcurrentAllow(t *testing.T) {
	limiter := New(1000, 1000, time.Minute)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				limiter.Allow("shared")
			}
		}()
	}
	wg.Wait()
	if got := len(limiter.buckets); got != 1 {
		t.Fatalf("bucket count = %d; want 1", got)
	}
}
