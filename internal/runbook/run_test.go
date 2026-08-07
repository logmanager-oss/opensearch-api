package runbook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// recordedCall is one request captured by scriptedServer.
type recordedCall struct {
	method string
	query  url.Values
	header http.Header
	body   []byte
}

// pathScript scripts a path's per-request status/body sequence; once
// requests outnumber the script, the last entry repeats.
type pathScript struct {
	statuses []int
	bodies   []string
}

// scriptedServer is a mux-based httptest server keyed by URL path, recording
// every request (in receipt order, across all paths) so runbook tests can
// assert both per-path detail and cross-path ordering.
type scriptedServer struct {
	mu      sync.Mutex
	order   []string
	byPath  map[string][]recordedCall
	scripts map[string]pathScript
}

func newScriptedServer(t *testing.T, scripts map[string]pathScript) (*httptest.Server, *scriptedServer) {
	t.Helper()
	ss := &scriptedServer{byPath: make(map[string][]recordedCall), scripts: scripts}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		ss.mu.Lock()
		idx := len(ss.byPath[r.URL.Path])
		ss.byPath[r.URL.Path] = append(ss.byPath[r.URL.Path], recordedCall{
			method: r.Method,
			query:  r.URL.Query(),
			header: r.Header.Clone(),
			body:   body,
		})
		ss.order = append(ss.order, r.URL.Path)
		script := ss.scripts[r.URL.Path]
		ss.mu.Unlock()

		code := http.StatusOK
		if len(script.statuses) > 0 {
			code = script.statuses[clampIdx(idx, len(script.statuses))]
		}
		respBody := ""
		if len(script.bodies) > 0 {
			respBody = script.bodies[clampIdx(idx, len(script.bodies))]
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, ss
}

func clampIdx(idx, n int) int {
	if idx >= n {
		return n - 1
	}
	return idx
}

func (ss *scriptedServer) requests(path string) []recordedCall {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.byPath[path]
}

func (ss *scriptedServer) count(path string) int {
	return len(ss.requests(path))
}

func mustLoad(t *testing.T, doc string) *Runbook {
	t.Helper()
	rb, err := Load(strings.NewReader(doc), "")
	require.NoError(t, err)
	return rb
}

func TestRunEntryOrder(t *testing.T) {
	srv, ss := newScriptedServer(t, nil)
	rb := mustLoad(t, `
calls:
  first:
    path: /a
  second:
    path: /b
  third:
    path: /c
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Equal(t, []string{"/a", "/b", "/c"}, ss.order)
}

func TestRunFailedCallContinuesAndAggregates(t *testing.T) {
	srv, ss := newScriptedServer(t, map[string]pathScript{
		"/a": {statuses: []int{http.StatusInternalServerError}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
  b:
    path: /b
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrRetriesExhausted)
	assert.Equal(t, 1, ss.count("/b"), "the run continues past the failed call")
	assert.Contains(t, progress.String(), `call "a": failed`)
}

func TestRunStopOnFailure(t *testing.T) {
	srv, ss := newScriptedServer(t, map[string]pathScript{
		"/a": {statuses: []int{http.StatusInternalServerError}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    stop-on-failure: true
  b:
    path: /b
  c:
    path: /c
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.Error(t, err)
	assert.Equal(t, 0, ss.count("/b"), "stop-on-failure halts before b")
	assert.Equal(t, 0, ss.count("/c"), "stop-on-failure halts before c")
	assert.Contains(t, progress.String(), `call "b": skipped`)
	assert.Contains(t, progress.String(), `call "c": skipped`)
}

func TestRunDependsOnSkipCascade(t *testing.T) {
	srv, ss := newScriptedServer(t, map[string]pathScript{
		"/a": {statuses: []int{http.StatusInternalServerError}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
  b:
    path: /b
    depends-on: a
  c:
    path: /c
    depends-on: b
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.Error(t, err)
	assert.Equal(t, 0, ss.count("/b"), "b's prerequisite a failed")
	assert.Equal(t, 0, ss.count("/c"), "the skip cascades from b to c")
	assert.Contains(t, progress.String(), `call "b": skipped (needs a)`)
	assert.Contains(t, progress.String(), `call "c": skipped (needs b)`)
	assert.Contains(t, progress.String(), "run: 0 succeeded, 1 failed, 2 skipped")
}

func TestRunDependsOnSatisfied(t *testing.T) {
	srv, ss := newScriptedServer(t, nil)
	rb := mustLoad(t, `
calls:
  a:
    path: /a
  b:
    path: /b
  c:
    path: /c
    depends-on: [a, b]
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Equal(t, 1, ss.count("/c"))
}

func TestRunAllSucceed(t *testing.T) {
	srv, _ := newScriptedServer(t, nil)
	rb := mustLoad(t, `
calls:
  a:
    path: /a
  b:
    path: /b
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Contains(t, progress.String(), "run: 2 succeeded, 0 failed, 0 skipped")
}

func TestRunAbortOnTerminal(t *testing.T) {
	srv, ss := newScriptedServer(t, map[string]pathScript{
		"/a": {statuses: []int{http.StatusConflict}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    retry: 5
    abort-on: [409]
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrTerminalStatus)
	assert.Equal(t, 1, ss.count("/a"), "abort-on stops after a single attempt")
	assert.Contains(t, progress.String(), `call "a": failed (status 409, terminal)`)
}

// cancelingTransport cancels the run's context on its first RoundTrip and
// fails as if the in-flight request observed that cancellation, mirroring
// what a real transport does when its context is canceled mid-request.
type cancelingTransport struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancelingTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	c.calls++
	c.cancel()
	return nil, context.Canceled
}

func TestRunContextCanceled(t *testing.T) {
	rb := mustLoad(t, `
calls:
  a:
    path: /a
  b:
    path: /b
`)

	ctx, cancel := context.WithCancel(context.Background())
	transport := &cancelingTransport{cancel: cancel}
	var progress strings.Builder
	r := &Runner{
		Client:   &http.Client{Transport: transport},
		Endpoint: "http://runbook-test.invalid",
		Progress: &progress,
		Warn:     io.Discard,
	}

	err := r.Run(ctx, rb)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, transport.calls, "no call after the canceling one is attempted")
	assert.NotContains(t, progress.String(), "run:", "a canceled run must not print a summary line")
}

func TestRunRequestDetails(t *testing.T) {
	srv, ss := newScriptedServer(t, nil)
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    method: POST
    body: '{"x":1}'
    query:
      size: "5"
    headers:
      X-Custom: hi
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)
	require.NoError(t, err)

	reqs := ss.requests("/a")
	require.Len(t, reqs, 1)
	got := reqs[0]
	assert.Equal(t, http.MethodPost, got.method)
	assert.Equal(t, "5", got.query.Get("size"))
	assert.Equal(t, "hi", got.header.Get("X-Custom"))
	assert.Equal(t, `{"x":1}`, string(got.body))
}

func TestRunAttemptCount(t *testing.T) {
	srv, _ := newScriptedServer(t, map[string]pathScript{
		"/a": {statuses: []int{
			http.StatusServiceUnavailable,
			http.StatusServiceUnavailable,
			http.StatusOK,
		}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    retry: 2
    backoff-initial: "1ms"
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Contains(t, progress.String(), `call "a": ok (status 200, 3 attempts)`)
}

func TestRunVerifyWithPassesFirstTry(t *testing.T) {
	srv, ss := newScriptedServer(t, nil)
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    verify-with: check_a
  check_a:
    path: /check
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Equal(t, 1, ss.count("/a"))
	assert.Equal(t, 1, ss.count("/check"))

	out := progress.String()
	startIdx := strings.Index(out, `call "a": GET /a`)
	checkIdx := strings.Index(out, `  check "check_a": ok (status 200, 1 attempt)`)
	okIdx := strings.Index(out, `call "a": ok (status 200, 1 attempt)`)
	require.GreaterOrEqual(t, startIdx, 0, out)
	require.GreaterOrEqual(t, checkIdx, 0, out)
	require.GreaterOrEqual(t, okIdx, 0, out)
	assert.True(t, startIdx < checkIdx && checkIdx < okIdx, out)
}

func TestRunVerifyWithFailsThenOuterRetrySucceeds(t *testing.T) {
	srv, ss := newScriptedServer(t, map[string]pathScript{
		"/check": {statuses: []int{http.StatusInternalServerError, http.StatusOK}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    retry: 1
    backoff-initial: "1ms"
    verify-with: check_a
  check_a:
    path: /check
    retry: 0
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Equal(t, 2, ss.count("/a"), "the outer call retries once after the check first fails")
	assert.Equal(t, 2, ss.count("/check"))
}

func TestRunVerifyWithExhaustsThenOuterRetrySucceeds(t *testing.T) {
	srv, ss := newScriptedServer(t, map[string]pathScript{
		"/check": {statuses: []int{
			http.StatusInternalServerError,
			http.StatusInternalServerError,
			http.StatusOK,
		}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    retry: 1
    backoff-initial: "1ms"
    verify-with: check_a
  check_a:
    path: /check
    retry: 1
    backoff-initial: "1ms"
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard, Verbose: true}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Equal(t, 2, ss.count("/a"))
	assert.Equal(t, 3, ss.count("/check"), "the check exhausts 2 attempts on the first invocation, then succeeds on the first attempt of the second")
	assert.Contains(t, progress.String(), `  check "check_a": attempt 1: status 500; retrying in`,
		"nested verbose attempt lines carry the indent and check label")
}

func TestRunVerifyWithAbortOnPropagatesTerminal(t *testing.T) {
	srv, ss := newScriptedServer(t, map[string]pathScript{
		"/check": {statuses: []int{http.StatusConflict}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    verify-with: check_a
  check_a:
    path: /check
    retry: 5
    abort-on: [409]
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrTerminalStatus)
	assert.Equal(t, 1, ss.count("/a"), "the outer call fails on its first and only attempt")
	assert.Equal(t, 1, ss.count("/check"), "the check's own abort-on fires on its first attempt")
	assert.Contains(t, progress.String(), `  check "check_a": failed (status 409, terminal)`)
	assert.Contains(t, progress.String(), `call "a": failed (check terminal)`,
		"the outer line must not claim the call's own status was terminal")
	assert.Contains(t, err.Error(), `check "check_a"`, "the error chain labels the check as a check")
}

func TestRunVerifyWithSkippedOnNonSuccessOuterResponse(t *testing.T) {
	srv, ss := newScriptedServer(t, map[string]pathScript{
		"/a": {statuses: []int{http.StatusInternalServerError, http.StatusOK}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    retry: 1
    backoff-initial: "1ms"
    verify-with: check_a
  check_a:
    path: /check
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Equal(t, 2, ss.count("/a"))
	assert.Equal(t, 1, ss.count("/check"), "the check is only invoked for the 2xx outer attempt")
}

// cancelAfterTransport cancels the run's context once requests to path reach
// n, simulating an in-flight cancellation inside a nested check's unlimited
// retry loop; requests to any other path are forwarded to inner untouched.
type cancelAfterTransport struct {
	cancel context.CancelFunc
	inner  http.RoundTripper
	path   string
	n      int
	calls  int
}

func (c *cancelAfterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != c.path {
		return c.inner.RoundTrip(req)
	}
	c.calls++
	if c.calls >= c.n {
		c.cancel()
		return nil, context.Canceled
	}
	return c.inner.RoundTrip(req)
}

// A deterministic check failure (its request cannot even be built) must
// propagate and fail the outer call immediately — never be treated as "not
// yet" by an unlimited outer retry.
func TestRunVerifyWithBuildErrorFailsFast(t *testing.T) {
	srv, ss := newScriptedServer(t, nil)
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    retry: -1
    backoff-initial: "1ms"
    verify-with: check_a
  check_a:
    path: "/%zz"
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.Error(t, err)
	assert.NotErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), `check "check_a"`)
	assert.Equal(t, 1, ss.count("/a"), "the outer call must not retry a permanently broken check")
}

func TestRunVerifyWithCancelDuringUnlimitedRetry(t *testing.T) {
	srv, _ := newScriptedServer(t, map[string]pathScript{
		"/check": {statuses: []int{http.StatusInternalServerError}},
	})
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    verify-with: check_a
  check_a:
    path: /check
    retry: -1
    backoff-initial: "1ms"
`)

	ctx, cancel := context.WithCancel(context.Background())
	transport := &cancelAfterTransport{cancel: cancel, inner: http.DefaultTransport, path: "/check", n: 3}
	var progress strings.Builder
	r := &Runner{Client: &http.Client{Transport: transport}, Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(ctx, rb)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRunVerifyWithChain(t *testing.T) {
	srv, ss := newScriptedServer(t, nil)
	rb := mustLoad(t, `
calls:
  a:
    path: /a
    verify-with: b
  b:
    path: /b
    verify-with: c
  c:
    path: /c
`)

	var progress strings.Builder
	r := &Runner{Client: srv.Client(), Endpoint: srv.URL, Progress: &progress, Warn: io.Discard}
	err := r.Run(context.Background(), rb)

	require.NoError(t, err)
	assert.Equal(t, 1, ss.count("/a"))
	assert.Equal(t, 1, ss.count("/b"))
	assert.Equal(t, 1, ss.count("/c"))
	assert.Contains(t, progress.String(), `    check "c": ok (status 200, 1 attempt)`)
	assert.Contains(t, progress.String(), `  check "b": ok (status 200, 1 attempt)`)
}

func TestRunVerboseAttemptLines(t *testing.T) {
	tests := []struct {
		name    string
		verbose bool
	}{
		{name: "verbose on shows per-attempt lines", verbose: true},
		{name: "verbose off shows only the outcome line", verbose: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newScriptedServer(t, map[string]pathScript{
				"/a": {statuses: []int{http.StatusServiceUnavailable, http.StatusOK}},
			})
			rb := mustLoad(t, `
calls:
  a:
    path: /a
    retry: 1
    backoff-initial: "1ms"
`)

			var progress strings.Builder
			r := &Runner{
				Client: srv.Client(), Endpoint: srv.URL,
				Progress: &progress, Warn: io.Discard, Verbose: tt.verbose,
			}
			err := r.Run(context.Background(), rb)
			require.NoError(t, err)

			if tt.verbose {
				assert.Contains(t, progress.String(), `call "a": attempt 1`)
			} else {
				assert.NotContains(t, progress.String(), "attempt 1")
			}
		})
	}
}
