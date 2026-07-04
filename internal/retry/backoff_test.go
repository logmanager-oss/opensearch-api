package retry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

func TestDuration(t *testing.T) {
	const initial = 2 * time.Second

	tests := []struct {
		name    string
		cfg     config.RetryConfig
		attempt int
		jitter  func() float64
		want    time.Duration
	}{
		{
			name:    "constant attempt 1",
			cfg:     config.RetryConfig{Strategy: config.Constant, Initial: initial},
			attempt: 1,
			want:    initial,
		},
		{
			name:    "constant attempt 5",
			cfg:     config.RetryConfig{Strategy: config.Constant, Initial: initial},
			attempt: 5,
			want:    initial,
		},
		{
			name:    "linear attempt 1",
			cfg:     config.RetryConfig{Strategy: config.Linear, Initial: initial},
			attempt: 1,
			want:    initial,
		},
		{
			name:    "linear attempt 3",
			cfg:     config.RetryConfig{Strategy: config.Linear, Initial: initial},
			attempt: 3,
			want:    6 * time.Second,
		},
		{
			name:    "exponential attempt 1",
			cfg:     config.RetryConfig{Strategy: config.Exponential, Initial: initial},
			attempt: 1,
			want:    initial,
		},
		{
			name:    "exponential attempt 4",
			cfg:     config.RetryConfig{Strategy: config.Exponential, Initial: initial},
			attempt: 4,
			want:    16 * time.Second,
		},
		{
			name:    "exponential capped at max",
			cfg:     config.RetryConfig{Strategy: config.Exponential, Initial: initial, Max: 10 * time.Second},
			attempt: 5,
			want:    10 * time.Second,
		},
		{
			name:    "linear capped at max",
			cfg:     config.RetryConfig{Strategy: config.Linear, Initial: initial, Max: 5 * time.Second},
			attempt: 10,
			want:    5 * time.Second,
		},
		{
			name:    "large exponential attempt does not overflow",
			cfg:     config.RetryConfig{Strategy: config.Exponential, Initial: initial, Max: 30 * time.Second},
			attempt: 100,
			want:    30 * time.Second,
		},
		{
			name:    "uncapped exponential overflow guard yields max duration",
			cfg:     config.RetryConfig{Strategy: config.Exponential, Initial: initial},
			attempt: 100,
			want:    maxDuration,
		},
		{
			name:    "uncapped linear overflow guard yields max duration",
			cfg:     config.RetryConfig{Strategy: config.Linear, Initial: maxDuration / 50},
			attempt: 100,
			want:    maxDuration,
		},
		{
			name:    "jitter midpoint yields base",
			cfg:     config.RetryConfig{Strategy: config.Constant, Initial: initial, Jitter: 0.5},
			attempt: 1,
			jitter:  func() float64 { return 0.5 },
			want:    initial,
		},
		{
			name:    "jitter low bound",
			cfg:     config.RetryConfig{Strategy: config.Constant, Initial: initial, Jitter: 0.25},
			attempt: 1,
			jitter:  func() float64 { return 0 },
			want:    time.Duration(float64(initial) * 0.75),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Duration(tt.cfg, tt.attempt, tt.jitter)
			assert.Equal(t, tt.want, got)
			assert.Positive(t, got, "backoff must never overflow to a non-positive value")
		})
	}
}

func TestDurationJitterWithinBounds(t *testing.T) {
	const initial = 4 * time.Second
	const j = 0.3
	cfg := config.RetryConfig{Strategy: config.Constant, Initial: initial, Jitter: j}

	lo := time.Duration(float64(initial) * (1 - j))
	hi := time.Duration(float64(initial) * (1 + j))

	for _, v := range []float64{0, 0.1, 0.4, 0.5, 0.9, 0.999} {
		got := Duration(cfg, 1, func() float64 { return v })
		assert.GreaterOrEqual(t, got, lo, "jitter=%v", v)
		assert.LessOrEqual(t, got, hi, "jitter=%v", v)
	}
}

// TestDurationJitterFractionClamped guards against the busy-loop footgun: a
// Jitter fraction >= 1 must not let the delay collapse to zero.
func TestDurationJitterFractionClamped(t *testing.T) {
	const initial = 8 * time.Second
	const maxDelay = 16 * time.Second
	cfg := config.RetryConfig{Strategy: config.Constant, Initial: initial, Max: maxDelay, Jitter: 1.5}

	for _, v := range []float64{0, 0.5, 0.999} {
		got := Duration(cfg, 1, func() float64 { return v })
		assert.Positive(t, got, "jitter sample=%v must stay positive", v)
		assert.LessOrEqual(t, got, maxDelay, "jitter sample=%v must stay within Max", v)
	}
}

// TestDurationJitterReclampAboveMax ensures upward jitter is re-clamped to Max.
func TestDurationJitterReclampAboveMax(t *testing.T) {
	const maxDelay = 10 * time.Second
	cfg := config.RetryConfig{Strategy: config.Constant, Initial: maxDelay, Max: maxDelay, Jitter: 0.5}

	got := Duration(cfg, 1, func() float64 { return 1.0 }) // out-of-range sample pushing up
	assert.Equal(t, maxDelay, got)
}
