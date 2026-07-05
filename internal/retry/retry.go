package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

// Sentinel errors returned by Engine.Do, wrapped with context via %w.
var (
	// ErrTerminalStatus is returned when a response has a terminal (abort-on) status.
	ErrTerminalStatus = errors.New("terminal status")
	// ErrRetriesExhausted is returned when the retry budget is exhausted.
	ErrRetriesExhausted = errors.New("retries exhausted")
)

// Attempt performs a single request. Implementations must honour ctx.
type Attempt func(ctx context.Context) (*http.Response, error)

// RetryInfo describes a retry about to happen; passed to the OnRetry hook.
type RetryInfo struct {
	Attempt int           // 1-based number of the attempt that just failed
	Status  int           // response status, or 0 on transport error
	Err     error         // transport error, or nil
	Delay   time.Duration // backoff before the next attempt
}

// Engine drives a retry loop with configurable outcome classification and
// backoff. It is safe for reuse across calls.
type Engine struct {
	cfg     config.RetryConfig
	sleep   func(context.Context, time.Duration) error
	jitter  func() float64
	onRetry func(RetryInfo)
}

// Option configures an Engine.
type Option func(*Engine)

// WithSleep injects a sleep function (defaults to a context-aware timer).
func WithSleep(fn func(context.Context, time.Duration) error) Option {
	return func(e *Engine) { e.sleep = fn }
}

// WithJitter injects a jitter source returning values in [0,1) (defaults to a
// seeded PRNG; only consulted when cfg.Jitter > 0).
func WithJitter(fn func() float64) Option {
	return func(e *Engine) { e.jitter = fn }
}

// WithOnRetry sets a nil-safe hook fired before each backoff.
func WithOnRetry(fn func(RetryInfo)) Option {
	return func(e *Engine) { e.onRetry = fn }
}

// New builds an Engine for cfg with the given options.
//
//nolint:gocritic // hugeParam: RetryConfig passed by value by design (small, immutable).
func New(cfg config.RetryConfig, opts ...Option) *Engine {
	e := &Engine{cfg: cfg}
	for _, opt := range opts {
		opt(e)
	}
	if e.sleep == nil {
		e.sleep = timerSleep
	}
	if e.jitter == nil {
		e.jitter = rand.Float64 // top-level source: auto-seeded and goroutine-safe.
	}
	return e
}

// Do runs attempt until success, a terminal status, attempt exhaustion, or
// context cancellation. On success, a terminal status, or attempt exhaustion the
// final response body is left open for the caller to read and close (a transport
// error leaves it nil); intermediate retried bodies are drained and closed.
// The Engine and attempt must be non-nil.
func (e *Engine) Do(ctx context.Context, attempt Attempt) (*http.Response, error) {
	for n := 1; ; n++ {
		resp, err := attempt(ctx)

		outcome := Retry
		if err != nil {
			if ctxErr := contextError(ctx, err); ctxErr != nil {
				drainStatus(resp) // never leak a response returned alongside a ctx error
				return nil, ctxErr
			}
		} else {
			outcome = classify(e.cfg, resp.StatusCode, nil)
		}

		switch outcome {
		case Success:
			return resp, nil
		case Terminal:
			return resp, fmt.Errorf("terminal status %d: %w", resp.StatusCode, ErrTerminalStatus)
		}

		// outcome == Retry from here on.
		if ctxErr := ctx.Err(); ctxErr != nil {
			drainStatus(resp)
			return nil, ctxErr
		}
		// n attempts done so far means n-1 retries; stop once the retry budget
		// (MaxRetries, <0 = unlimited) is used up.
		if e.cfg.MaxRetries >= 0 && n > e.cfg.MaxRetries {
			// Exhausted: hand the final response back with its body open (like
			// Terminal) so the caller can still read it. A transport error
			// leaves resp nil, so there is no body to return.
			return resp, fmt.Errorf("after %d attempts: %w", n, ErrRetriesExhausted)
		}

		status := drainStatus(resp)
		delay := Duration(e.cfg, n, e.jitter)
		if e.onRetry != nil {
			e.onRetry(RetryInfo{Attempt: n, Status: status, Err: err, Delay: delay})
		}
		if err := e.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// contextError returns the context error if ctx is done or err is a context
// error, meaning the loop must propagate instead of retrying.
func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// drainStatus drains and closes a retryable response body to allow connection
// reuse, returning its status code (0 when resp is nil).
func drainStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func timerSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
