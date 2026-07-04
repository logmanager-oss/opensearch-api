package retry

import (
	"math"
	"time"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

const maxDuration = time.Duration(math.MaxInt64)

// maxJitterFraction caps the effective jitter fraction strictly below 1 so the
// backoff factor stays in (0, 2): a positive base delay can never collapse to
// zero and trigger a hot retry loop.
const maxJitterFraction = 0.999

// Duration returns the backoff delay before a 1-based attempt (attempt 1 is the
// first backoff after the first failure). It applies the configured strategy,
// caps at cfg.Max, and applies optional symmetric jitter using the injected
// jitter source (expected in [0,1)). A nil jitter or cfg.Jitter==0 means no
// jitter. Both the jitter fraction and the sample are defensively clamped so a
// misconfigured Jitter or out-of-range source cannot drive the delay to zero.
//
//nolint:gocritic // hugeParam: RetryConfig passed by value by design (small, immutable).
func Duration(cfg config.RetryConfig, attempt int, jitter func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := clampMax(cfg.Max, baseDelay(&cfg, attempt))
	if cfg.Jitter <= 0 || jitter == nil {
		return d
	}
	fraction := clampUnit(cfg.Jitter)
	sample := clampUnit(jitter())
	factor := 1 - fraction + sample*2*fraction
	d = time.Duration(float64(d) * factor)
	if d < 0 {
		d = 0
	}
	return clampMax(cfg.Max, d)
}

func baseDelay(cfg *config.RetryConfig, attempt int) time.Duration {
	switch cfg.Strategy {
	case config.Linear:
		return linearDelay(cfg.Initial, attempt)
	case config.Exponential:
		return expDelay(cfg.Initial, attempt)
	case config.Constant:
		return cfg.Initial
	default:
		return cfg.Initial
	}
}

func linearDelay(initial time.Duration, attempt int) time.Duration {
	if initial <= 0 {
		return initial
	}
	if int64(attempt) > int64(maxDuration)/int64(initial) {
		return maxDuration
	}
	return initial * time.Duration(attempt)
}

func expDelay(initial time.Duration, attempt int) time.Duration {
	if initial <= 0 {
		return initial
	}
	d := initial
	for i := 1; i < attempt; i++ {
		if d > maxDuration/2 {
			return maxDuration
		}
		d *= 2
	}
	return d
}

func clampMax(maxDelay, d time.Duration) time.Duration {
	if maxDelay > 0 && d > maxDelay {
		return maxDelay
	}
	return d
}

// clampUnit clamps v into [0, maxJitterFraction], keeping it inside [0, 1).
func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > maxJitterFraction {
		return maxJitterFraction
	}
	return v
}
