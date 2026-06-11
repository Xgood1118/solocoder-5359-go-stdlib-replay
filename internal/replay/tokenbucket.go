package replay

import (
	"sync"
	"time"
)

type TokenBucket struct {
	capacity    float64
	tokens      float64
	refillRate  float64
	lastRefill  time.Time
	mu          sync.Mutex
}

func NewTokenBucket(rps float64) *TokenBucket {
	return &TokenBucket{
		capacity:   rps,
		tokens:     rps,
		refillRate: rps,
		lastRefill: time.Now(),
	}
}

func (tb *TokenBucket) Take() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.refillRate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	tb.lastRefill = now

	if tb.tokens >= 1 {
		tb.tokens -= 1
		return true
	}
	return false
}

func (tb *TokenBucket) Wait() {
	for {
		if tb.Take() {
			return
		}
		waitTime := time.Duration(float64(time.Second) / tb.refillRate)
		time.Sleep(waitTime)
	}
}
