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
