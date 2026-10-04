package ratelimit

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

type Keyed struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rate     rate.Limit
	burst    int
	maxKeys  int
}

func NewKeyed(r float64, burst int, maxKeys int) *Keyed {
	return &Keyed{
		limiters: make(map[string]*rate.Limiter),
		rate:     rate.Limit(r),
		burst:    burst,
		maxKeys:  maxKeys,
	}
}

func (k *Keyed) Wait(ctx context.Context, key string) error {
	k.mu.Lock()
	l, exists := k.limiters[key]
	if !exists {
		if len(k.limiters) >= k.maxKeys {
			// Clear half to prevent memory exhaustion
			for keyToRemove := range k.limiters {
				delete(k.limiters, keyToRemove)
				if len(k.limiters) <= k.maxKeys/2 {
					break
				}
			}
		}
		l = rate.NewLimiter(k.rate, k.burst)
		k.limiters[key] = l
	}
	k.mu.Unlock()

	return l.Wait(ctx)
}
