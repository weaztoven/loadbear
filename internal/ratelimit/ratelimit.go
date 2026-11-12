package ratelimit

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	Rate   float64
	Burst  int
}

func (b *Bucket) consume() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.last.IsZero() {
		b.last = now
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.Rate
	if b.tokens > float64(b.Burst) {
		b.tokens = float64(b.Burst)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*Bucket
	Rate    float64
	Burst   int
	Window  time.Duration

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

func New(rate float64, burst int, window time.Duration) *RateLimiter {
	ctx, cancel := context.WithCancel(context.Background())
	return &RateLimiter{
		buckets: make(map[string]*Bucket),
		Rate:    rate,
		Burst:   burst,
		Window:  window,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
}

func (r *RateLimiter) Start(interval time.Duration) error {
	if interval <= 0 {
		return errors.New("eviction interval must be positive")
	}
	started := false
	r.startOnce.Do(func() {
		started = true
		go func() {
			defer close(r.done)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-r.ctx.Done():
					return
				case <-ticker.C:
					r.EvictExpired()
				}
			}
		}()
	})
	if !started {
		return errors.New("rate limiter eviction has already been started or stopped")
	}
	return nil
}

func (r *RateLimiter) Stop() {
	r.stopOnce.Do(func() {
		r.cancel()
		r.startOnce.Do(func() {
			close(r.done)
		})
		<-r.done
	})
}

func (r *RateLimiter) Allow(ip string) (bool, int) {
	if ip == "" {
		ip = "unknown"
	}
	r.mu.Lock()
	bucket, ok := r.buckets[ip]
	if !ok {
		bucket = &Bucket{Rate: r.Rate, Burst: r.Burst, last: time.Now(), tokens: float64(r.Burst)}
		r.buckets[ip] = bucket
	}
	r.mu.Unlock()
	if bucket.consume() {
		return true, 0
	}
	return false, int(r.Window.Seconds())
}

func (r *RateLimiter) EvictExpired() {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for ip, bucket := range r.buckets {
		bucket.mu.Lock()
		stale := now.Sub(bucket.last) > r.Window
		bucket.mu.Unlock()
		if stale {
			delete(r.buckets, ip)
		}
	}
}
