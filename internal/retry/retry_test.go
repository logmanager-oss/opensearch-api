package retry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

// scriptServer returns an httptest server replying with the i-th status from
// statuses (clamped to the last), plus an atomic call counter.
func scriptServer(t *testing.T, statuses ...int) (srv *httptest.Server, counter *int32) {
	t.Helper()
	var n int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&n, 1))
		code := statuses[len(statuses)-1]
		if i <= len(statuses) {
			code = statuses[i-1]
		}
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "attempt %d", i)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func serverAttempt(srv *httptest.Server) Attempt {
	return func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
		if err != nil {
			return nil, err
		}
		return srv.Client().Do(req)
	}
}

func recordingSleep() (func(context.Context, time.Duration) error, *[]time.Duration) {
	var delays []time.Duration
	fn := func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	return fn, &delays
}

func fixedRetryCfg() config.RetryConfig {
	// MaxRetries: -1 = unlimited, so tests retry until success/terminal unless
	// they override it.
	return config.RetryConfig{MaxRetries: -1, Strategy: config.Constant, Initial: time.Second}
}

func TestEngineDoSuccessAfterRetries(t *testing.T) {
	srv, counter := scriptServer(t, 503, 503, 200)
	sleep, delays := recordingSleep()
	e := New(fixedRetryCfg(), WithSleep(sleep))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(3), atomic.LoadInt32(counter))
	assert.Len(t, *delays, 2)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "attempt 3", string(body))
}

func TestEngineDoTerminalStatus(t *testing.T) {
	srv, counter := scriptServer(t, 409)
	sleep, delays := recordingSleep()
	cfg := fixedRetryCfg()
	cfg.AbortOn = []int{409}
	e := New(cfg, WithSleep(sleep))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.Error(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	assert.ErrorIs(t, err, ErrTerminalStatus)
	assert.Equal(t, int32(1), atomic.LoadInt32(counter))
	assert.Empty(t, *delays)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "attempt 1", string(body), "terminal body must stay readable")
}

func TestEngineDoTerminalStatus404(t *testing.T) {
	t.Run("404 in terminal status stops with terminal error", func(t *testing.T) {
		srv, counter := scriptServer(t, 404)
		sleep, _ := recordingSleep()
		cfg := fixedRetryCfg()
		cfg.AbortOn = []int{409, 404}
		e := New(cfg, WithSleep(sleep))

		resp, err := e.Do(context.Background(), serverAttempt(srv))
		require.Error(t, err)
		require.NotNil(t, resp)
		defer func() { _ = resp.Body.Close() }()
		assert.ErrorIs(t, err, ErrTerminalStatus)
		assert.Equal(t, int32(1), atomic.LoadInt32(counter))
	})

	t.Run("404 retried by default", func(t *testing.T) {
		srv, counter := scriptServer(t, 404, 404, 200)
		sleep, _ := recordingSleep()
		e := New(fixedRetryCfg(), WithSleep(sleep))

		resp, err := e.Do(context.Background(), serverAttempt(srv))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, int32(3), atomic.LoadInt32(counter))
	})
}

func TestEngineDoTransportErrorThenSuccess(t *testing.T) {
	srv, _ := scriptServer(t, 200)
	sleep, delays := recordingSleep()
	e := New(fixedRetryCfg(), WithSleep(sleep))

	var n int32
	attempt := func(ctx context.Context) (*http.Response, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return nil, errors.New("dial tcp: connection refused")
		}
		return serverAttempt(srv)(ctx)
	}

	resp, err := e.Do(context.Background(), attempt)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, *delays, 1)
}

func TestEngineDoRetriesExhausted(t *testing.T) {
	srv, counter := scriptServer(t, 503)
	sleep, delays := recordingSleep()
	cfg := fixedRetryCfg()
	cfg.MaxRetries = 2 // 2 retries => 3 attempts total
	e := New(cfg, WithSleep(sleep))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.Error(t, err)
	require.NotNil(t, resp) // final response returned with body open
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	assert.Equal(t, "attempt 3", string(body))
	assert.ErrorIs(t, err, ErrRetriesExhausted)
	assert.Contains(t, err.Error(), "after 3 attempts")
	assert.Equal(t, int32(3), atomic.LoadInt32(counter))
	assert.Len(t, *delays, 2)
}

func TestEngineDoTransportErrorExhaustedWrapsCause(t *testing.T) {
	sleep, delays := recordingSleep()
	cfg := fixedRetryCfg()
	cfg.MaxRetries = 2 // 2 retries => 3 attempts total
	e := New(cfg, WithSleep(sleep))

	cause := errors.New("dial tcp: connection refused")
	attempt := func(context.Context) (*http.Response, error) { return nil, cause }

	resp, err := e.Do(context.Background(), attempt) //nolint:bodyclose // transport error returns a nil response
	require.Error(t, err)
	assert.Nil(t, resp)                         // transport error leaves no response
	assert.ErrorIs(t, err, ErrRetriesExhausted) // sentinel still matchable
	assert.ErrorIs(t, err, cause)               // transport cause surfaced
	assert.Contains(t, err.Error(), "after 3 attempts")
	assert.Contains(t, err.Error(), "connection refused")
	assert.Len(t, *delays, 2)
}

func TestEngineDoOnRetryHook(t *testing.T) {
	srv, _ := scriptServer(t, 503, 503, 200)
	sleep, _ := recordingSleep()
	var infos []RetryInfo
	e := New(fixedRetryCfg(), WithSleep(sleep), WithOnRetry(func(ri RetryInfo) {
		infos = append(infos, ri)
	}))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Len(t, infos, 2)
	assert.Equal(t, 1, infos[0].Attempt)
	assert.Equal(t, http.StatusServiceUnavailable, infos[0].Status)
	assert.Equal(t, 2, infos[1].Attempt)
}

// trackBody records whether it was closed, to assert retried bodies are drained.
type trackBody struct {
	*bytes.Reader
	closed bool
}

func (b *trackBody) Close() error {
	b.closed = true
	return nil
}

func TestEngineDoDrainsRetriedBodies(t *testing.T) {
	sleep, _ := recordingSleep()
	e := New(fixedRetryCfg(), WithSleep(sleep))

	first := &trackBody{Reader: bytes.NewReader([]byte("retry-body"))}
	var n int32
	attempt := func(_ context.Context) (*http.Response, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return &http.Response{StatusCode: 503, Body: first}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}

	resp, err := e.Do(context.Background(), attempt)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.True(t, first.closed, "retried body must be closed")
	rest, err := io.ReadAll(first.Reader)
	require.NoError(t, err)
	assert.Empty(t, rest, "retried body must be fully drained")
}

func TestEngineDoContextCancelMidBackoff(t *testing.T) {
	srv, counter := scriptServer(t, 503)
	cfg := config.RetryConfig{MaxRetries: -1, Strategy: config.Constant, Initial: 10 * time.Second}
	e := New(cfg) // real context-aware sleep

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	resp, err := e.Do(ctx, serverAttempt(srv)) //nolint:bodyclose // resp is nil on the context-cancel error path.
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 2*time.Second, "must return promptly on cancel")
	assert.Equal(t, int32(1), atomic.LoadInt32(counter), "must not run another attempt")
}

// Item 1: a response returned alongside a context error must be drained/closed,
// not leaked, and must not be handed back to the caller.
func TestEngineDoDrainsResponseOnContextError(t *testing.T) {
	sleep, _ := recordingSleep()
	e := New(fixedRetryCfg(), WithSleep(sleep))

	body := &trackBody{Reader: bytes.NewReader([]byte("leaked"))}
	attempt := func(_ context.Context) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: body}, context.Canceled
	}

	resp, err := e.Do(context.Background(), attempt) //nolint:bodyclose // resp is nil on the context-error path.
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, body.closed, "response returned with a context error must be closed")
}

// Item 4: WithSleep(nil) must fall back to the default sleep, not panic.
func TestEngineDoNilSleepFallsBack(t *testing.T) {
	srv, counter := scriptServer(t, 503, 200)
	cfg := config.RetryConfig{MaxRetries: 1, Strategy: config.Constant, Initial: time.Millisecond}
	e := New(cfg, WithSleep(nil))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), atomic.LoadInt32(counter))
}

// Item 5: if the context is cancelled after a retryable attempt but before the
// backoff, the hook must not fire and Do must return the context error.
func TestEngineDoNoHookWhenCancelledBeforeSleep(t *testing.T) {
	sleep, delays := recordingSleep()
	var infos []RetryInfo
	e := New(fixedRetryCfg(), WithSleep(sleep), WithOnRetry(func(ri RetryInfo) {
		infos = append(infos, ri)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	var n int32
	attempt := func(_ context.Context) (*http.Response, error) {
		atomic.AddInt32(&n, 1)
		cancel() // cancel before Do reaches the retry/backoff path
		return &http.Response{StatusCode: 503, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}

	resp, err := e.Do(ctx, attempt) //nolint:bodyclose // resp is nil on the context-cancel error path.
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, infos, "hook must not fire when already cancelled")
	assert.Empty(t, *delays, "must not sleep when already cancelled")
	assert.Equal(t, int32(1), atomic.LoadInt32(&n))
}

// MaxRetries=0 (the default) means exactly one attempt and no retry.
func TestEngineDoNoRetry(t *testing.T) {
	srv, counter := scriptServer(t, 503)
	sleep, delays := recordingSleep()
	cfg := fixedRetryCfg()
	cfg.MaxRetries = 0
	e := New(cfg, WithSleep(sleep))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.Error(t, err)
	require.NotNil(t, resp) // single attempt still returns the response body
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.ErrorIs(t, err, ErrRetriesExhausted)
	assert.Contains(t, err.Error(), "after 1 attempts")
	assert.Equal(t, int32(1), atomic.LoadInt32(counter))
	assert.Empty(t, *delays)
}

// Item 8: a context error surfaced as the attempt's transport error (while
// ctx.Err() may be nil) is propagated, not retried.
func TestEngineDoTransportContextErrorPropagates(t *testing.T) {
	sleep, delays := recordingSleep()
	e := New(fixedRetryCfg(), WithSleep(sleep))

	var n int32
	attempt := func(_ context.Context) (*http.Response, error) {
		atomic.AddInt32(&n, 1)
		return nil, context.DeadlineExceeded
	}

	resp, err := e.Do(context.Background(), attempt) //nolint:bodyclose // resp is nil on the context-error path.
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int32(1), atomic.LoadInt32(&n), "must not retry a context error")
	assert.Empty(t, *delays)
}
