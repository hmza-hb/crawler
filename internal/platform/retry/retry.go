package retry

import (
	"context"
	"math/rand"
	"time"
)

type Policy struct {
	MaxAttempts int
	Base        time.Duration
	Max         time.Duration
	Multiplier  float64
	Jitter      float64
}

func DoValue[T any](
	ctx context.Context,
	p Policy,
	isRetryable func(error) bool,
	fn func(ctx context.Context, attempt int) (T, error),
) (T, error) {
	var zero T
	attempts := p.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}

	backoff := p.Base
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}

	multiplier := p.Multiplier
	if multiplier <= 1.0 {
		multiplier = 2.0
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		val, err := fn(ctx, attempt)
		if err == nil {
			return val, nil
		}

		if attempt == attempts || (isRetryable != nil && !isRetryable(err)) {
			return val, err
		}

		delay := backoff
		if p.Jitter > 0 {
			jitterDelta := float64(delay) * p.Jitter * (rand.Float64()*2 - 1)
			delay = time.Duration(float64(delay) + jitterDelta)
		}

		if p.Max > 0 && delay > p.Max {
			delay = p.Max
		}

		select {
		case <-ctx.Done():
			return val, ctx.Err()
		case <-time.After(delay):
		}

		backoff = time.Duration(float64(backoff) * multiplier)
	}

	return zero, ctx.Err()
}
