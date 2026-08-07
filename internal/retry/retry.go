package retry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
	Reason  string        // classification reason (e.g. "--retry-when matched"); "" for plain status classification
}

// Engine drives a retry loop with configurable outcome classification and
// backoff. It is safe for reuse across calls.
type Engine struct {
	cfg          config.RetryConfig
	sleep        func(context.Context, time.Duration) error
	jitter       func() float64
	onRetry      func(RetryInfo)
	retryWhen    *Predicate
	successWhen  *Predicate
	warn         io.Writer
	successCheck func(context.Context) (bool, error)
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

// WithRetryWhen sets the compiled --retry-when predicate (nil disables it).
func WithRetryWhen(p *Predicate) Option {
	return func(e *Engine) { e.retryWhen = p }
}

// WithSuccessWhen sets the compiled --success-when predicate (nil disables it).
func WithSuccessWhen(p *Predicate) Option {
	return func(e *Engine) { e.successWhen = p }
}

// WithWarn sets the writer for per-attempt predicate warnings (defaults to io.Discard).
func WithWarn(w io.Writer) Option {
	return func(e *Engine) { e.warn = w }
}

// WithSuccessCheck sets an external success check consulted only on a 2xx
// response, after abort-on and retry-when. nil disables it. Mutually
// exclusive with WithSuccessWhen (enforced by New).
func WithSuccessCheck(fn func(ctx context.Context) (bool, error)) Option {
	return func(e *Engine) { e.successCheck = fn }
}

// hasPredicates reports whether any body predicate is configured — the gate
// for buffering bodies in Do and for evaluating predicates in classify, which
// must always agree.
func (e *Engine) hasPredicates() bool {
	return e.retryWhen != nil || e.successWhen != nil
}

// New builds an Engine for cfg with the given options. Setting both
// WithSuccessWhen and WithSuccessCheck is an error: classify defines no
// ordering between them.
//
//nolint:gocritic // hugeParam: RetryConfig passed by value by design (small, immutable).
func New(cfg config.RetryConfig, opts ...Option) (*Engine, error) {
	e := &Engine{cfg: cfg}
	for _, opt := range opts {
		opt(e)
	}
	if e.successWhen != nil && e.successCheck != nil {
		return nil, errors.New("WithSuccessWhen and WithSuccessCheck are mutually exclusive")
	}
	if e.sleep == nil {
		e.sleep = timerSleep
	}
	if e.jitter == nil {
		e.jitter = rand.Float64 // top-level source: auto-seeded and goroutine-safe.
	}
	if e.warn == nil {
		e.warn = io.Discard
	}
	return e, nil
}

// Do runs attempt until success, a terminal status, attempt exhaustion, or
// context cancellation. On success, a terminal status, or attempt exhaustion the
// final response body is left open for the caller to read and close (a transport
// error, or a failure reading the body for predicate evaluation, leaves it nil);
// intermediate retried bodies are drained and closed (bodies buffered for
// predicate evaluation just close: the socket is at EOF, or deliberately
// abandoned when over-cap rather than draining a possibly huge remainder). Attempt
// exhaustion wraps the final classification reason (e.g. "--success-when not
// satisfied") when one applies. The Engine and attempt must be non-nil.
func (e *Engine) Do(ctx context.Context, attempt Attempt) (*http.Response, error) {
	for n := 1; ; n++ {
		resp, err := attempt(ctx)

		var body []byte
		var buffered, overflowed bool
		if err == nil && e.hasPredicates() {
			body, overflowed, err = bufferBody(resp, e.cfg.MaxBodyBuffer)
			buffered = err == nil
			if err != nil {
				_ = resp.Body.Close()
				resp = nil
				err = fmt.Errorf("reading response body: %w", err)
			}
		}

		if err != nil {
			if ctxErr := contextError(ctx, err); ctxErr != nil {
				drainStatus(resp) // never leak a response returned alongside a ctx error
				return nil, ctxErr
			}
		}

		respStatus := 0
		if resp != nil {
			respStatus = resp.StatusCode
		}
		outcome, reason, classifyErr := e.classify(ctx, respStatus, body, overflowed, err)
		if classifyErr != nil {
			closeAttempt(resp, buffered)
			return nil, classifyErr
		}

		switch outcome {
		case Success:
			return resp, nil
		case Terminal:
			return resp, fmt.Errorf("terminal status %d: %w", resp.StatusCode, ErrTerminalStatus)
		}

		// outcome == Retry from here on.
		if ctxErr := ctx.Err(); ctxErr != nil {
			closeAttempt(resp, buffered)
			return nil, ctxErr
		}
		// n attempts done so far means n-1 retries; stop once the retry budget
		// (MaxRetries, <0 = unlimited) is used up.
		if e.cfg.MaxRetries >= 0 && n > e.cfg.MaxRetries {
			// Exhausted: hand the final response back with its body open (like
			// Terminal) so the caller can still read it. A transport (or
			// body-read) error leaves resp nil (no body), so surface its cause in
			// the chain — both ErrRetriesExhausted and the cause stay
			// errors.Is-matchable.
			if err != nil {
				drainStatus(resp) // nil on a transport/body-read error, but never leak a body if one is returned
				return nil, fmt.Errorf("after %d attempts: %w: %w", n, ErrRetriesExhausted, err)
			}
			if reason != "" {
				return resp, fmt.Errorf("after %d attempts: %w: %s", n, ErrRetriesExhausted, reason)
			}
			return resp, fmt.Errorf("after %d attempts: %w", n, ErrRetriesExhausted)
		}

		status := closeAttempt(resp, buffered)
		delay := Duration(e.cfg, n, e.jitter)
		if e.onRetry != nil {
			e.onRetry(RetryInfo{Attempt: n, Status: status, Err: err, Delay: delay, Reason: reason})
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

// closeAttempt closes a retryable response body, returning its status code
// (0 when resp is nil). A body that went through bufferBody (buffered=true
// implies a non-nil resp) needs no drain before closing: the socket is either
// already at EOF (fully buffered) or deliberately abandoned (an over-cap
// remainder may be arbitrarily large). Unbuffered bodies drain for connection
// reuse, as today.
func closeAttempt(resp *http.Response, buffered bool) int {
	if !buffered {
		return drainStatus(resp)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// bufferBody reads up to maxBuffer+1 bytes of resp's body for predicate
// evaluation and reassembles resp.Body as the buffered prefix followed by the
// unread remainder, so the caller-visible body is never truncated. overflowed
// reports that more than maxBuffer bytes were available (buf is nil in that
// case: its content is irrelevant once predicates are skipped). maxBuffer<=0
// (and MaxInt64, where the +1 would overflow) means unlimited: a plain
// io.ReadAll.
func bufferBody(resp *http.Response, maxBuffer int64) (buf []byte, overflowed bool, err error) {
	orig := resp.Body
	var read []byte
	if maxBuffer <= 0 || maxBuffer == math.MaxInt64 {
		read, err = io.ReadAll(orig)
	} else {
		read, err = io.ReadAll(io.LimitReader(orig, maxBuffer+1))
		overflowed = int64(len(read)) > maxBuffer
	}
	if err != nil {
		return nil, false, err
	}
	resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(read), orig), orig: orig}
	if overflowed {
		return nil, true, nil
	}
	return read, false, nil
}

// readCloser stitches a buffered body prefix to the unread socket remainder,
// closing the original body exactly once.
type readCloser struct {
	io.Reader
	orig io.ReadCloser
}

func (rc *readCloser) Close() error {
	return rc.orig.Close()
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
