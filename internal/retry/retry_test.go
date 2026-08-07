package retry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

// scriptedServer returns an httptest server whose per-attempt response comes
// from script (called with the 1-based attempt number), plus an atomic call
// counter.
func scriptedServer(t *testing.T, script func(i int) (status int, body string)) (srv *httptest.Server, counter *int32) {
	t.Helper()
	var n int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&n, 1))
		status, body := script(i)
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// scriptServer replies with the i-th status from statuses (clamped to the
// last) and an "attempt %d" body carrying the live attempt number.
func scriptServer(t *testing.T, statuses ...int) (srv *httptest.Server, counter *int32) {
	t.Helper()
	return scriptedServer(t, func(i int) (int, string) {
		return statuses[min(i, len(statuses))-1], fmt.Sprintf("attempt %d", i)
	})
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

// attemptScript is one scripted per-attempt response for scriptServerWithBodies.
type attemptScript struct {
	status int
	body   string
}

// scriptServerWithBodies is scriptServer's cousin for tests that also need
// per-attempt bodies (e.g. predicate evaluation), clamped to the last script.
func scriptServerWithBodies(t *testing.T, attempts ...attemptScript) (srv *httptest.Server, counter *int32) {
	t.Helper()
	return scriptedServer(t, func(i int) (int, string) {
		a := attempts[min(i, len(attempts))-1]
		return a.status, a.body
	})
}

// mustNew builds an Engine, failing the test on an invalid option combination.
//
//nolint:gocritic // hugeParam: RetryConfig passed by value to mirror New.
func mustNew(t *testing.T, cfg config.RetryConfig, opts ...Option) *Engine {
	t.Helper()
	e, err := New(cfg, opts...)
	require.NoError(t, err)
	return e
}

func fixedRetryCfg() config.RetryConfig {
	// MaxRetries: -1 = unlimited, so tests retry until success/terminal unless
	// they override it.
	return config.RetryConfig{MaxRetries: -1, Strategy: config.Constant, Initial: time.Second}
}

func TestEngineDoSuccessAfterRetries(t *testing.T) {
	srv, counter := scriptServer(t, 503, 503, 200)
	sleep, delays := recordingSleep()
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep))

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
	e := mustNew(t, cfg, WithSleep(sleep))

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
		e := mustNew(t, cfg, WithSleep(sleep))

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
		e := mustNew(t, fixedRetryCfg(), WithSleep(sleep))

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
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep))

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
	e := mustNew(t, cfg, WithSleep(sleep))

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
	e := mustNew(t, cfg, WithSleep(sleep))

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
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep), WithOnRetry(func(ri RetryInfo) {
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
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep))

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
	e := mustNew(t, cfg) // real context-aware sleep

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
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep))

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
	e := mustNew(t, cfg, WithSleep(nil))

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
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep), WithOnRetry(func(ri RetryInfo) {
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
	e := mustNew(t, cfg, WithSleep(sleep))

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
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep))

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

func TestEngineDoRetryWhenThenSuccess(t *testing.T) {
	srv, counter := scriptServerWithBodies(t,
		attemptScript{status: 200, body: `{"retry":true}`},
		attemptScript{status: 200, body: `{"retry":false}`},
	)
	sleep, delays := recordingSleep()
	retryWhen, err := CompilePredicate(".retry")
	require.NoError(t, err)
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep), WithRetryWhen(retryWhen))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, int32(2), atomic.LoadInt32(counter))
	assert.Len(t, *delays, 1)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"retry":false}`, string(body))
}

func TestEngineDoSuccessWhenExhaustionCarriesReason(t *testing.T) {
	srv, _ := scriptServerWithBodies(t, attemptScript{status: 200, body: `{"ok":false}`})
	sleep, _ := recordingSleep()
	cfg := fixedRetryCfg()
	cfg.MaxRetries = 1 // 2 attempts total
	successWhen, err := CompilePredicate(".ok")
	require.NoError(t, err)
	e := mustNew(t, cfg, WithSleep(sleep), WithSuccessWhen(successWhen))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.Error(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()
	assert.ErrorIs(t, err, ErrRetriesExhausted)
	assert.Contains(t, err.Error(), "after 2 attempts")
	assert.Contains(t, err.Error(), "--success-when not satisfied")
}

func TestEngineDoAbortOnBeatsRetryWhen(t *testing.T) {
	srv, counter := scriptServerWithBodies(t, attemptScript{status: 409, body: `{}`})
	sleep, _ := recordingSleep()
	cfg := fixedRetryCfg()
	cfg.AbortOn = []int{409}
	retryWhen, err := CompilePredicate("true")
	require.NoError(t, err)
	e := mustNew(t, cfg, WithSleep(sleep), WithRetryWhen(retryWhen))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.Error(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()
	assert.ErrorIs(t, err, ErrTerminalStatus)
	assert.Equal(t, int32(1), atomic.LoadInt32(counter))
}

func TestEngineDoWarnsAndRecordsReason(t *testing.T) {
	srv, _ := scriptServerWithBodies(t,
		attemptScript{status: 200, body: "not json"},
		attemptScript{status: 200, body: `{"ok":true}`},
	)
	sleep, _ := recordingSleep()
	var buf bytes.Buffer
	var infos []RetryInfo
	successWhen, err := CompilePredicate(".ok")
	require.NoError(t, err)
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep), WithSuccessWhen(successWhen), WithWarn(&buf),
		WithOnRetry(func(ri RetryInfo) { infos = append(infos, ri) }))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.NotEmpty(t, buf.String())
	require.Len(t, infos, 1)
	assert.Equal(t, "--success-when not satisfied", infos[0].Reason)
}

func TestEngineDoCapOverflowKeepsFullBodyReadable(t *testing.T) {
	pad := strings.Repeat("a", 100)
	body := fmt.Sprintf(`{"pad":%q}`, pad)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	// RetryWhen alone (no SuccessWhen): an overflowed, unevaluated body falls
	// through to the plain status-based default instead of forcing a retry.
	retryWhen, err := CompilePredicate(".retry")
	require.NoError(t, err)
	cfg := fixedRetryCfg()
	cfg.MaxBodyBuffer = 16
	e := mustNew(t, cfg, WithRetryWhen(retryWhen), WithWarn(&buf))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err) // status 200: falls back to status-based Success once overflow skips the predicate
	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "body must not be truncated by the buffer cap")
	assert.Contains(t, buf.String(), "--max-body-buffer")
}

func TestEngineDoRetriedOverflowClosedWithoutDraining(t *testing.T) {
	sleep, _ := recordingSleep()
	cfg := fixedRetryCfg()
	cfg.MaxBodyBuffer = 20 // overflows the 1000-byte first body but not the 11-byte second one
	successWhen, err := CompilePredicate(".ok")
	require.NoError(t, err)
	e := mustNew(t, cfg, WithSleep(sleep), WithSuccessWhen(successWhen))

	large := strings.Repeat("x", 1000)
	first := &trackBody{Reader: bytes.NewReader([]byte(large))}
	var n int32
	attempt := func(_ context.Context) (*http.Response, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return &http.Response{StatusCode: 200, Body: first}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`)))}, nil
	}

	resp, err := e.Do(context.Background(), attempt)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.True(t, first.closed, "overflowed retried body must be closed")
	remaining, err := io.ReadAll(first.Reader)
	require.NoError(t, err)
	assert.NotEmpty(t, remaining, "overflowed retried body must not be drained")
}

// A cap of MaxInt64 must take the unlimited path: naively computing cap+1 for
// the LimitReader would overflow to a negative limit and read nothing.
func TestBufferBodyMaxInt64CapIsUnlimited(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`)))}

	buf, overflowed, err := bufferBody(resp, math.MaxInt64)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.False(t, overflowed)
	assert.JSONEq(t, `{"ok":true}`, string(buf))
}

func TestEngineDoMaxBodyBufferZeroIsUnlimited(t *testing.T) {
	pad := strings.Repeat("a", 100000)
	body := fmt.Sprintf(`{"pad":%q,"ok":true}`, pad)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	successWhen, err := CompilePredicate(".ok")
	require.NoError(t, err)
	cfg := fixedRetryCfg()
	cfg.MaxBodyBuffer = 0
	e := mustNew(t, cfg, WithSuccessWhen(successWhen), WithWarn(&buf))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Empty(t, buf.String(), "unlimited buffer must not warn")
}

func TestEngineDoSuccessWhenOnNon2xx(t *testing.T) {
	srv, counter := scriptServerWithBodies(t, attemptScript{status: 503, body: `{"ok":true}`})
	sleep, _ := recordingSleep()
	successWhen, err := CompilePredicate(".ok")
	require.NoError(t, err)
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep), WithSuccessWhen(successWhen))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, int32(1), atomic.LoadInt32(counter))
}

func TestEngineDoBodyReadErrorTreatedAsTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(srv.Close)

	sleep, _ := recordingSleep()
	successWhen, err := CompilePredicate(".ok")
	require.NoError(t, err)
	cfg := fixedRetryCfg()
	cfg.MaxRetries = 1
	e := mustNew(t, cfg, WithSleep(sleep), WithSuccessWhen(successWhen))

	resp, err := e.Do(context.Background(), serverAttempt(srv)) //nolint:bodyclose // resp is nil on the body-read-error path.
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, ErrRetriesExhausted)
	assert.Contains(t, err.Error(), "reading response body")
	assert.Contains(t, err.Error(), "after 2 attempts")
}

func TestEngineDoSuccessCheckPasses(t *testing.T) {
	srv, counter := scriptServer(t, 200)
	sleep, _ := recordingSleep()
	check := func(context.Context) (bool, error) { return true, nil }
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep), WithSuccessCheck(check))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(1), atomic.LoadInt32(counter))
}

func TestEngineDoSuccessCheckExhaustionCarriesReason(t *testing.T) {
	srv, _ := scriptServer(t, 200)
	sleep, _ := recordingSleep()
	cfg := fixedRetryCfg()
	cfg.MaxRetries = 1 // 2 attempts total
	check := func(context.Context) (bool, error) { return false, nil }
	e := mustNew(t, cfg, WithSleep(sleep), WithSuccessCheck(check))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.Error(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()
	assert.ErrorIs(t, err, ErrRetriesExhausted)
	assert.Contains(t, err.Error(), "after 2 attempts")
	assert.Contains(t, err.Error(), "success check failed")
}

func TestEngineDoSuccessCheckNotInvokedOnNon2xx(t *testing.T) {
	srv, counter := scriptServer(t, 503, 503, 200)
	sleep, _ := recordingSleep()
	var checkCalls int32
	check := func(context.Context) (bool, error) {
		atomic.AddInt32(&checkCalls, 1)
		return true, nil
	}
	e := mustNew(t, fixedRetryCfg(), WithSleep(sleep), WithSuccessCheck(check))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, int32(3), atomic.LoadInt32(counter))
	assert.Equal(t, int32(1), atomic.LoadInt32(&checkCalls), "check must run only on the final 2xx attempt")
}

func TestEngineDoSuccessCheckErrorStopsImmediately(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		wantIs error
	}{
		{
			name:   "wrapped terminal status",
			err:    fmt.Errorf("nested verify: %w", ErrTerminalStatus),
			wantIs: ErrTerminalStatus,
		},
		{
			name:   "context canceled",
			err:    context.Canceled,
			wantIs: context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, counter := scriptServer(t, 200, 200)
			sleep, _ := recordingSleep()
			check := func(context.Context) (bool, error) { return false, tt.err }
			e := mustNew(t, fixedRetryCfg(), WithSleep(sleep), WithSuccessCheck(check))

			resp, err := e.Do(context.Background(), serverAttempt(srv)) //nolint:bodyclose // resp is nil on the propagate-error path.
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.ErrorIs(t, err, tt.wantIs)
			assert.Equal(t, int32(1), atomic.LoadInt32(counter), "must not attempt again once the check propagates an error")
		})
	}
}

func TestEngineDoSuccessCheckOnlyDoesNotBufferBody(t *testing.T) {
	body := `{"status":"ok"}`
	srv, _ := scriptServerWithBodies(t, attemptScript{status: 200, body: body})

	check := func(context.Context) (bool, error) { return true, nil }
	e := mustNew(t, fixedRetryCfg(), WithSuccessCheck(check))

	resp, err := e.Do(context.Background(), serverAttempt(srv))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
}

func TestNewRejectsSuccessWhenWithSuccessCheck(t *testing.T) {
	successWhen, err := CompilePredicate(".ok")
	require.NoError(t, err)
	check := func(context.Context) (bool, error) { return true, nil }

	e, err := New(fixedRetryCfg(), WithSuccessWhen(successWhen), WithSuccessCheck(check))
	require.Error(t, err)
	assert.Nil(t, e)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// repeat(null) never terminates on its own; a non-terminating predicate must
// still make Do return promptly once ctx is done, not hang.
func TestEngineDoContextCancelledDuringPredicateEvaluation(t *testing.T) {
	srv, counter := scriptServerWithBodies(t, attemptScript{status: 200, body: `{}`})
	retryWhen, err := CompilePredicate("repeat(null)")
	require.NoError(t, err)
	e := mustNew(t, fixedRetryCfg(), WithRetryWhen(retryWhen))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	var resp *http.Response
	var doErr error
	go func() {
		resp, doErr = e.Do(ctx, serverAttempt(srv)) //nolint:bodyclose // resp is nil on the context-error path.
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Do did not return promptly on a cancelled context")
	}

	require.Error(t, doErr)
	assert.Nil(t, resp)
	assert.ErrorIs(t, doErr, context.DeadlineExceeded)
	assert.Equal(t, int32(1), atomic.LoadInt32(counter), "the request itself must have completed before the timeout")
}
