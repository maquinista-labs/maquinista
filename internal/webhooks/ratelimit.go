package webhooks

import (
	"sync"
	"time"
)

// bucket is a simple per-handler token bucket: capacity tokens per minute,
// refills linearly.
type bucket struct {
	mu        sync.Mutex
	capacity  float64
	tokens    float64
	lastCheck time.Time
}

func (s *Server) allow(handlerID string, ratePerMin int, now time.Time) bool {
	if ratePerMin <= 0 {
		return true
	}
	v, _ := s.buckets.LoadOrStore(handlerID, &bucket{
		capacity:  float64(ratePerMin),
		tokens:    float64(ratePerMin),
		lastCheck: now,
	})
	b := v.(*bucket)
	b.mu.Lock()
	defer b.mu.Unlock()

	elapsed := now.Sub(b.lastCheck).Seconds()
	b.tokens += elapsed * (b.capacity / 60.0)
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastCheck = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
