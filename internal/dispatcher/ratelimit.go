package dispatcher

import (
	"context"
	"sync"
	"time"
)

// tokenBucket paces calls to Telegram. Tokens replenish at `ratePerSec` per
// second up to a burst of `ratePerSec`. A zero rate disables throttling.
type tokenBucket struct {
	mu         sync.Mutex
	rate       int
	capacity   int
	tokens     float64
	lastRefill time.Time
}

func newTokenBucket(ratePerSec int) *tokenBucket {
	if ratePerSec <= 0 {
		return nil
	}
	return &tokenBucket{
		rate:       ratePerSec,
		capacity:   ratePerSec,
		tokens:     float64(ratePerSec),
		lastRefill: time.Now(),
	}
}

// Wait blocks until a token is available or ctx is done.
func (b *tokenBucket) Wait(ctx context.Context) {
	if b == nil {
		return
	}
	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.lastRefill).Seconds()
		b.tokens += elapsed * float64(b.rate)
		if b.tokens > float64(b.capacity) {
			b.tokens = float64(b.capacity)
		}
		b.lastRefill = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return
		}
		needed := 1 - b.tokens
		wait := time.Duration(needed / float64(b.rate) * float64(time.Second))
		b.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
