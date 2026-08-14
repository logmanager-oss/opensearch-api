package runbook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

type recordedCall struct {
	method string
	path   string
	query  url.Values
	header http.Header
	body   []byte
}

// recorder records requests in arrival order, guarded for concurrent handlers.
type recorder struct {
	mu   sync.Mutex
	reqs []recordedCall
}

func (c *recorder) add(r recordedCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
}

func (c *recorder) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func (c *recorder) at(i int) recordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs[i]
}

func (c *recorder) paths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.reqs))
	for i, r := range c.reqs {
		out[i] = r.path
	}
	return out
}

// pathScript is a per-path scripted response sequence: statuses[i]/bodies[i]
// for the i-th request to that path (each clamped to the last).
type pathScript struct {
	statuses []int
	bodies   []string
}

// newMuxServer serves scripts keyed by path, recording every request into
// rec. Runbook calls hit different paths (unlike the single-endpoint cli
// tests), so routing is a mux rather than one handler.
func newMuxServer(t *testing.T, rec *recorder, scripts map[string]*pathScript) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var mu sync.Mutex
	counts := make(map[string]int, len(scripts))
	for path, script := range scripts {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			rec.add(recordedCall{
				method: r.Method,
				path:   r.URL.Path,
				query:  r.URL.Query(),
				header: r.Header.Clone(),
				body:   body,
			})

			mu.Lock()
			idx := counts[path]
			counts[path]++
			mu.Unlock()

			status := script.statuses[clampIdx(idx, len(script.statuses))]
			respBody := ""
			if len(script.bodies) > 0 {
				respBody = script.bodies[clampIdx(idx, len(script.bodies))]
			}
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, respBody)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func clampIdx(idx, length int) int {
	if idx >= length {
		return length - 1
	}
	return idx
}

func TestRunAllSucceed(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/first":  {statuses: []int{http.StatusOK}},
		"/second": {statuses: []int{http.StatusOK}},
		"/third":  {statuses: []int{http.StatusOK}},
	})

	src := `
calls:
  - name: first
    method: GET
    path: /first
  - name: second
    method: GET
    path: /second
  - name: third
    method: GET
    path: /third
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	err = runner.Run(context.Background(), rb)
	require.NoError(t, err)

	// calls execute in document order.
	assert.Equal(t, []string{"/first", "/second", "/third"}, rec.paths())

	// all-success summary.
	assert.Contains(t, stderr.String(), `run: 3 succeeded`)
}

func TestRunHaltsOnFailure(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/first":  {statuses: []int{http.StatusOK}},
		"/second": {statuses: []int{http.StatusBadRequest}, bodies: []string{`{"error":"bad"}`}},
		"/third":  {statuses: []int{http.StatusOK}},
		"/fourth": {statuses: []int{http.StatusOK}},
	})

	src := `
calls:
  - name: first
    path: /first
  - name: second
    path: /second
    abort-on: [400]
  - name: third
    path: /third
  - name: fourth
    path: /fourth
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	err = runner.Run(context.Background(), rb)

	// a failing call halts the run; later calls receive no requests.
	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrTerminalStatus, "Run returns the halting call's error")
	assert.ErrorContains(t, err, `call "second"`, "wrapped with the call name")
	assert.Equal(t, []string{"/first", "/second"}, rec.paths(), "third and fourth must not run")
	assert.Contains(t, stderr.String(), "run: 1 succeeded, 1 failed (halted), 2 not run")
}

func TestRunContinueOnFailure(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/first":  {statuses: []int{http.StatusOK}},
		"/second": {statuses: []int{http.StatusBadRequest}, bodies: []string{`{"error":"bad"}`}},
		"/third":  {statuses: []int{http.StatusOK}},
	})

	src := `
calls:
  - name: first
    path: /first
  - name: second
    path: /second
    abort-on: [400]
    continue-on-failure: true
  - name: third
    path: /third
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	err = runner.Run(context.Background(), rb)

	// continue-on-failure lets the run continue and return nil.
	require.NoError(t, err)
	assert.Equal(t, []string{"/first", "/second", "/third"}, rec.paths())
	out := stderr.String()
	// Anchored to the outcome line: the summary alone also contains "(tolerated)".
	assert.Contains(t, out, `call "second": failed (status 400, terminal, 1 attempt) (tolerated)`)
	assert.Contains(t, out, "run: 2 succeeded, 1 failed (tolerated)")
}

func TestRunFailingBodyEchoed(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/ok":        {statuses: []int{http.StatusOK}, bodies: []string{`{"ack":true}`}},
		"/fail":      {statuses: []int{http.StatusBadRequest}, bodies: []string{`{"error":"boom"}`}},
		"/tolerated": {statuses: []int{http.StatusBadRequest}, bodies: []string{`{"error":"tolerated-boom"}`}},
	})

	src := `
calls:
  - name: ok_call
    path: /ok
  - name: tolerated_call
    path: /tolerated
    abort-on: [400]
    continue-on-failure: true
  - name: fail_call
    path: /fail
    abort-on: [400]
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	_ = runner.Run(context.Background(), rb)

	out := stderr.String()
	// failing bodies (halting and tolerated) are echoed, indented;
	// a successful call's body is not.
	assert.Contains(t, out, `  {"error":"boom"}`)
	assert.Contains(t, out, `  {"error":"tolerated-boom"}`)
	assert.NotContains(t, out, `{"ack":true}`)
}

func TestRunFailingBodyTruncation(t *testing.T) {
	bigBody := strings.Repeat("a", 2048) // > 1KiB
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/capped":    {statuses: []int{http.StatusBadRequest}, bodies: []string{bigBody}},
		"/unlimited": {statuses: []int{http.StatusBadRequest}, bodies: []string{bigBody}},
	})

	src := `
calls:
  - name: capped
    path: /capped
    abort-on: [400]
    max-body-buffer: '1KiB'
    continue-on-failure: true
  - name: unlimited
    path: /unlimited
    abort-on: [400]
    max-body-buffer: '0'
    continue-on-failure: true
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	require.NoError(t, runner.Run(context.Background(), rb))

	out := stderr.String()
	// over-cap body is truncated to exactly 1KiB with a trailing
	// marker; the same call with max-body-buffer: 0 (the LimitReader(_, 0)
	// trap) prints the full 2KiB body untruncated.
	assert.Contains(t, out, strings.Repeat("a", 1024)+"\n  … (truncated at 1KiB)",
		"truncated content must stop at exactly 1KiB, not 1KiB+1")
	assert.Contains(t, out, bigBody, "max-body-buffer: 0 means unlimited")
	assert.Equal(t, 1, strings.Count(out, "truncated at"), "only the capped call should report truncation")
}

func TestRunSuccessWhenExhaustsWithReason(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/wait_green": {statuses: []int{http.StatusOK}, bodies: []string{`{"status":"yellow"}`}},
	})

	src := `
calls:
  - name: wait_green
    path: /wait_green
    success-when: '.status == "green"'
    retry: 3
    backoff-initial: '1ms'
    backoff-max: '1ms'
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	err = runner.Run(context.Background(), rb)

	// Behavior 8: success-when never satisfied retries to exhaustion and the
	// failure line carries the deciding reason.
	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrRetriesExhausted)
	assert.Equal(t, 4, rec.len(), "1 initial + 3 retries")
	assert.Contains(t, stderr.String(),
		`call "wait_green": failed (status 200, retries exhausted: --success-when not satisfied, 4 attempts)`)
}

func TestRunSuccessWhenAcceptsNonSuccessStatusWithoutRetry(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/maybe_missing": {statuses: []int{http.StatusNotFound}, bodies: []string{`{"status":404}`}},
	})

	src := `
calls:
  - name: maybe_missing
    path: /maybe_missing
    success-when: '.status == 404'
    retry: 5
    backoff-initial: '1ms'
    backoff-max: '1ms'
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	err = runner.Run(context.Background(), rb)

	// success-when accepting a non-2xx succeeds without retrying.
	require.NoError(t, err)
	assert.Equal(t, 1, rec.len())
	assert.Contains(t, stderr.String(), `call "maybe_missing": ok (status 404, 1 attempt)`)
}

func TestRunContextCanceledMidRun(t *testing.T) {
	var rec recorder
	gotRequest := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/first", func(w http.ResponseWriter, r *http.Request) {
		rec.add(recordedCall{method: r.Method, path: r.URL.Path})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/wait_green", func(w http.ResponseWriter, r *http.Request) {
		rec.add(recordedCall{method: r.Method, path: r.URL.Path})
		once.Do(func() { close(gotRequest) })
		<-r.Context().Done() // blocks until the client cancels, deterministically
	})
	mux.HandleFunc("/never", func(w http.ResponseWriter, r *http.Request) {
		rec.add(recordedCall{method: r.Method, path: r.URL.Path})
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	src := `
calls:
  - name: first
    path: /first
  - name: wait_green
    path: /wait_green
  - name: never_reached_a
    path: /never
  - name: never_reached_b
    path: /never
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		<-gotRequest
		cancel()
	}()

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	err = runner.Run(ctx, rb)

	// a canceled context returns promptly with an error matching
	// context.Canceled, the summary names the interrupted call and the
	// not-run count, and no further calls were attempted.
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"/first", "/wait_green"}, rec.paths())
	assert.Contains(t, stderr.String(), `run: 1 succeeded, interrupted during call "wait_green", 2 not run`)
}

func TestRunRequestDetailsReachServer(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/my-index/_doc/1": {statuses: []int{http.StatusOK}},
	})

	src := `
calls:
  - name: update_doc
    method: PUT
    path: /my-index/_doc/1
    body: '{"field":"value"}'
    query:
      if_seq_no: '5'
    headers:
      x-custom: hi
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	require.NoError(t, runner.Run(context.Background(), rb))

	// body, headers and query params from the YAML reach the server.
	require.Equal(t, 1, rec.len())
	got := rec.at(0)
	assert.Equal(t, "PUT", got.method)
	assert.Equal(t, "5", got.query.Get("if_seq_no"))
	assert.Equal(t, "hi", got.header.Get("X-Custom"))
	assert.Equal(t, `{"field":"value"}`, string(got.body))
}

func TestRunAttemptCountAndVerboseHook(t *testing.T) {
	src := `
calls:
  - name: flaky
    path: /flaky
    retry: 5
    backoff-initial: '1ms'
    backoff-max: '1ms'
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	newFlakyServer := func(t *testing.T) *httptest.Server {
		var rec recorder
		return newMuxServer(t, &rec, map[string]*pathScript{
			"/flaky": {statuses: []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusOK}},
		})
	}

	t.Run("quiet", func(t *testing.T) {
		srv := newFlakyServer(t)
		var stderr bytes.Buffer
		runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
		require.NoError(t, runner.Run(context.Background(), rb))

		// a call succeeding on its 3rd attempt
		// reports "3 attempts".
		out := stderr.String()
		assert.Contains(t, out, `call "flaky": ok (status 200, 3 attempts)`)
		// no per-attempt retry lines when quiet.
		assert.NotContains(t, out, "retrying in")
	})

	t.Run("verbose", func(t *testing.T) {
		srv := newFlakyServer(t)
		var stderr bytes.Buffer
		runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr, Verbose: true}
		require.NoError(t, runner.Run(context.Background(), rb))

		// per-attempt retry lines appear, prefixed
		// with the call name, only when Verbose.
		out := stderr.String()
		assert.Contains(t, out, `call "flaky": attempt 1: status 503; retrying in`)
		assert.Contains(t, out, `call "flaky": attempt 2: status 503; retrying in`)
	})
}

// callVerboseHook mirrors internal/cli/output.go's verboseHook, so each of
// its three format branches needs its own coverage: TestRunAttemptCountAndVerboseHook
// covers the plain-status branch; this covers the other two.
func TestRunVerboseHookReasonAndTransportError(t *testing.T) {
	t.Run("retry-when reason", func(t *testing.T) {
		var rec recorder
		srv := newMuxServer(t, &rec, map[string]*pathScript{
			"/status": {
				statuses: []int{http.StatusOK, http.StatusOK},
				bodies:   []string{`{"state":"pending"}`, `{"state":"done"}`},
			},
		})

		rb, err := Load(strings.NewReader(`
calls:
  - name: wait
    path: /status
    retry: 3
    backoff-initial: '1ms'
    retry-when: '.state == "pending"'
`), "")
		require.NoError(t, err)

		var stderr bytes.Buffer
		runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr, Verbose: true}
		require.NoError(t, runner.Run(context.Background(), rb))

		// Behavior: a retry-when-forced retry reports the deciding reason
		// alongside the status.
		assert.Contains(t, stderr.String(),
			`call "wait": attempt 1: status 200 (--retry-when matched); retrying in`)
	})

	t.Run("transport error", func(t *testing.T) {
		srv := newMuxServer(t, &recorder{}, map[string]*pathScript{"/x": {statuses: []int{http.StatusOK}}})
		endpoint := srv.URL
		srv.Close()

		rb, err := Load(strings.NewReader(`
calls:
  - name: ping
    path: /x
    retry: 1
    backoff-initial: '1ms'
`), "")
		require.NoError(t, err)

		var stderr bytes.Buffer
		runner := &Runner{Client: http.DefaultClient, Endpoint: endpoint, Stderr: &stderr, Verbose: true}
		err = runner.Run(context.Background(), rb)
		require.Error(t, err)

		// Behavior: a transport-error retry reports the raw error instead of a
		// status.
		out := stderr.String()
		assert.Contains(t, out, `call "ping": attempt 1 failed:`)
		assert.Contains(t, out, "; retrying in")
		assert.Contains(t, out, "connection refused")
	})
}

// runRunbook loads src and executes it against srv with a background context,
// returning the runner's stderr and Run's error. Tests needing cancellation or
// Verbose build their own Runner.
func runRunbook(t *testing.T, srv *httptest.Server, src string) (string, error) {
	t.Helper()
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	runErr := runner.Run(context.Background(), rb)
	return stderr.String(), runErr
}

// A retried call must resend its body: req.GetBody is replayed per attempt,
// and without it the second and third attempts would post nothing.
func TestRunRetryReplaysRequestBody(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/doc": {statuses: []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusOK}},
	})

	out, err := runRunbook(t, srv, `
calls:
  - name: index_doc
    method: POST
    path: /doc
    body: '{"field":"value"}'
    retry: 2
    backoff-initial: '1ms'
    backoff-max: '1ms'
`)
	require.NoError(t, err)
	require.Equal(t, 3, rec.len())
	for i := range 3 {
		assert.Equal(t, `{"field":"value"}`, string(rec.at(i).body), "attempt %d resends the body", i+1)
	}
	assert.Contains(t, out, `call "index_doc": ok (status 200, 3 attempts)`)
}

// max-body-buffer at exactly MaxInt64 means unlimited; the maxBuffer+1 bound
// would overflow negative and read nothing.
func TestRunFailingBodyMaxInt64IsUnlimited(t *testing.T) {
	big := strings.Repeat("x", 4096)
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/big": {statuses: []int{http.StatusBadRequest}, bodies: []string{big}},
	})

	out, err := runRunbook(t, srv, `
calls:
  - name: big
    path: /big
    abort-on: [400]
    max-body-buffer: '9223372036854775807'
`)
	require.Error(t, err)
	assert.Contains(t, out, big)
	assert.NotContains(t, out, "truncated at")
}

// retry-when forces a retry on a 2xx the engine would otherwise accept.
func TestRunRetryWhenForcesRetry(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/status": {
			statuses: []int{http.StatusOK, http.StatusOK},
			bodies:   []string{`{"state":"pending"}`, `{"state":"done"}`},
		},
	})

	out, err := runRunbook(t, srv, `
calls:
  - name: wait
    path: /status
    retry: 3
    backoff-initial: '1ms'
    retry-when: '.state == "pending"'
`)
	require.NoError(t, err)
	assert.Equal(t, 2, rec.len(), "the first 200 is retried because retry-when matched")
	assert.Contains(t, out, `call "wait": ok (status 200, 2 attempts)`)
}

// A transport error has no status and no body to echo.
func TestRunTransportError(t *testing.T) {
	srv := newMuxServer(t, &recorder{}, map[string]*pathScript{"/x": {statuses: []int{http.StatusOK}}})
	endpoint := srv.URL
	srv.Close()

	rb, err := Load(strings.NewReader(`
calls:
  - name: ping
    path: /x
    retry: 1
    backoff-initial: '1ms'
`), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: endpoint, Stderr: &stderr}
	err = runner.Run(context.Background(), rb)

	require.Error(t, err)
	out := stderr.String()
	assert.Contains(t, out, `call "ping": failed (transport error, 2 attempts): `)
	assert.Contains(t, out, "connect: connection refused")
	assert.NotContains(t, out, "retries exhausted", "the raw cause is shown, not the engine's wrapped text")
}

// A build-request failure never reaches the network, so it has no status,
// attempt count or body — but it must still be named, especially when
// tolerated, or the summary counts a failure the operator cannot identify.
func TestRunBuildRequestFailureIsReported(t *testing.T) {
	srv := newMuxServer(t, &recorder{}, map[string]*pathScript{"/fine": {statuses: []int{http.StatusOK}}})

	// An invalid URL escape in the path loads fine (load-time validation
	// cannot judge a path without the endpoint) but fails url.Parse in
	// BuildRequest — unlike an invalid method, which no longer survives Load.
	src := `
calls:
  - name: fine
    path: /fine
  - name: broken
    path: '/%zz'
    continue-on-failure: true
`
	out, err := runRunbook(t, srv, src)
	require.NoError(t, err, "tolerated, so the run still succeeds")
	assert.Contains(t, out, `call "broken": failed (request not sent) (tolerated): `)
	assert.Contains(t, out, "run: 1 succeeded, 1 failed (tolerated)")

	// The same failure untolerated halts the run and returns the error.
	out, err = runRunbook(t, srv, strings.Replace(src, "    continue-on-failure: true\n", "", 1))
	require.Error(t, err)
	assert.ErrorContains(t, err, `call "broken"`)
	assert.Contains(t, out, `call "broken": failed (request not sent): `)
}

// A halt after tolerated failures must account for every call.
func TestRunHaltSummaryCountsToleratedFailures(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/ok":    {statuses: []int{http.StatusOK}},
		"/tol":   {statuses: []int{http.StatusBadRequest}, bodies: []string{`{"e":1}`}},
		"/halt":  {statuses: []int{http.StatusBadRequest}, bodies: []string{`{"e":2}`}},
		"/never": {statuses: []int{http.StatusOK}},
	})

	out, err := runRunbook(t, srv, `
defaults:
  abort-on: [400]
calls:
  - name: ok
    path: /ok
  - name: tol
    path: /tol
    continue-on-failure: true
  - name: halt
    path: /halt
  - name: never
    path: /never
`)
	require.Error(t, err)
	assert.Equal(t, []string{"/ok", "/tol", "/halt"}, rec.paths())
	assert.Contains(t, out, "run: 1 succeeded, 1 failed (tolerated), 1 failed (halted), 1 not run")
}

// cancelAfterRT returns a canned response and then cancels, reproducing a
// SIGTERM landing just as a call finishes; the synthetic response keeps the
// cancellation from also breaking the body read.
type cancelAfterRT struct {
	status int
	body   string
	cancel context.CancelFunc
}

func (rt cancelAfterRT) RoundTrip(*http.Request) (*http.Response, error) {
	resp := &http.Response{
		StatusCode: rt.status,
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Header:     make(http.Header),
	}
	rt.cancel()
	return resp, nil
}

// A terminal failure that merely races cancellation must be reported as the
// failure it is: relabeling it "interrupted" would suppress its failure line
// and body echo while main.go still exits 1.
func TestRunTerminalFailureIsNotMistakenForCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rb, err := Load(strings.NewReader(`
calls:
  - name: index_doc
    path: /x
    abort-on: [400]
`), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{
		Client:   &http.Client{Transport: cancelAfterRT{status: http.StatusBadRequest, body: `{"error":"terminal"}`, cancel: cancel}},
		Endpoint: "http://example.invalid",
		Stderr:   &stderr,
	}
	err = runner.Run(ctx, rb)

	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrTerminalStatus)
	assert.NotErrorIs(t, err, context.Canceled, "exit code must stay 1, not 130")
	out := stderr.String()
	assert.Contains(t, out, `call "index_doc": failed (status 400, terminal, 1 attempt)`)
	assert.Contains(t, out, `{"error":"terminal"}`, "the body echo must not be suppressed")
	assert.NotContains(t, out, "interrupted")
}

// An http.Client Timeout error satisfies errors.Is(err, DeadlineExceeded)
// while the run's context is still alive; it must be reported as an ordinary
// call failure, not as the run being interrupted.
func TestRunClientTimeoutIsOrdinaryFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rb, err := Load(strings.NewReader(`
calls:
  - name: slow
    path: /slow
`), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{
		Client:   &http.Client{Timeout: 50 * time.Millisecond},
		Endpoint: srv.URL,
		Stderr:   &stderr,
	}
	err = runner.Run(context.Background(), rb)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `call "slow"`)
	out := stderr.String()
	assert.Contains(t, out, `call "slow": failed (transport error,`)
	assert.Contains(t, out, "run: 0 succeeded, 1 failed (halted), 0 not run")
	assert.NotContains(t, out, "interrupted")
}

// The echoed body is untrusted: a bare CR or an ANSI escape would let the
// endpoint overwrite the progress lines above it, including its own failure.
func TestRunFailingBodyStripsControlCharacters(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/evil": {
			statuses: []int{http.StatusBadRequest},
			bodies:   []string{"\x1b[2J\x1b[1;31mFAKE\x1b[0m\rrun: 99 succeeded\n"},
		},
	})

	out, err := runRunbook(t, srv, `
calls:
  - name: evil
    path: /evil
    abort-on: [400]
`)
	require.Error(t, err)
	assert.NotContains(t, out, "\x1b")
	assert.NotContains(t, out, "\r")
	assert.Contains(t, out, "  [2J[1;31mFAKE[0mrun: 99 succeeded\n")
	assert.Contains(t, out, "run: 0 succeeded, 1 failed (halted), 0 not run")
}

// A body-read failure during predicate evaluation leaves resp nil like a
// transport error does, but the request reached the server; the label must
// not send the operator chasing the network.
func TestRunBodyReadFailureNotLabeledTransportError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/truncated", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	out, err := runRunbook(t, srv, `
calls:
  - name: truncated
    path: /truncated
    success-when: '.x == 1'
`)

	require.Error(t, err)
	assert.Contains(t, out, `call "truncated": failed (retries exhausted`)
	assert.Contains(t, out, "reading response body")
	assert.NotContains(t, out, "transport error")
}

// When echoBody's read fails mid-stream, the partial body is still echoed and
// marked, distinguishing it from a complete short body.
func TestRunBodyEchoReadFailureMarked(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/truncated", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("partial"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	out, err := runRunbook(t, srv, `
calls:
  - name: truncated
    path: /truncated
    abort-on: [400]
`)

	require.Error(t, err)
	assert.Contains(t, out, `call "truncated": failed (status 400, terminal, 1 attempt)`)
	assert.Contains(t, out, "  partial")
	assert.Contains(t, out, "… (body read failed:")
}

// After a SIGTERM the summary is the only record of the run, so it must
// account for tolerated failures too, not just successes and not-run calls.
func TestRunCanceledSummaryCountsToleratedFailures(t *testing.T) {
	var rec recorder
	gotRequest := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/tol", func(w http.ResponseWriter, r *http.Request) {
		rec.add(recordedCall{path: r.URL.Path})
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"e":1}`)
	})
	mux.HandleFunc("/hang", func(_ http.ResponseWriter, r *http.Request) {
		rec.add(recordedCall{path: r.URL.Path})
		once.Do(func() { close(gotRequest) })
		<-r.Context().Done()
	})
	mux.HandleFunc("/never", func(w http.ResponseWriter, r *http.Request) {
		rec.add(recordedCall{path: r.URL.Path})
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rb, err := Load(strings.NewReader(`
calls:
  - name: tol
    path: /tol
    abort-on: [400]
    continue-on-failure: true
  - name: hang
    path: /hang
  - name: never
    path: /never
`), "")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		<-gotRequest
		cancel()
	}()

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
	err = runner.Run(ctx, rb)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"/tol", "/hang"}, rec.paths())
	assert.Contains(t, stderr.String(),
		`run: 0 succeeded, 1 failed (tolerated), interrupted during call "hang", 1 not run`)
}

// a value captured from call A appears in call B's path, query,
// header and body.
func TestRunCaptureSubstitutesEveryField(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read":    {statuses: []int{http.StatusOK}, bodies: []string{`{"seq":42}`}},
		"/next/42": {statuses: []int{http.StatusOK}},
	})

	_, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      seq: '.seq'
  - name: next
    method: PUT
    path: /next/${seq}
    query:
      if_seq_no: '${seq}'
    headers:
      x-seq: '${seq}'
    body: '{"seq":${seq}}'
`)
	require.NoError(t, err)
	require.Equal(t, 2, rec.len())
	got := rec.at(1)
	assert.Equal(t, "/next/42", got.path)
	assert.Equal(t, "42", got.query.Get("if_seq_no"))
	assert.Equal(t, "42", got.header.Get("X-Seq"))
	assert.Equal(t, `{"seq":42}`, string(got.body))
}

// string and int captures render correctly. This proves the wiring — a
// captured value reaching a later call's body — not rendering itself:
// renderScalar's branches are already covered by TestRenderScalar, so one
// row of each is enough.
func TestRunCaptureValueTypesRender(t *testing.T) {
	tests := []struct {
		name     string
		respBody string
		expr     string
		want     string
	}{
		{name: "string", respBody: `{"v":"hello"}`, expr: ".v", want: "hello"},
		{name: "int", respBody: `{"v":42}`, expr: ".v", want: "42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec recorder
			srv := newMuxServer(t, &rec, map[string]*pathScript{
				"/read": {statuses: []int{http.StatusOK}, bodies: []string{tt.respBody}},
				"/echo": {statuses: []int{http.StatusOK}},
			})

			src := fmt.Sprintf(`
calls:
  - name: read
    path: /read
    capture:
      v: '%s'
  - name: echo
    path: /echo
    body: '{"got":"${v}"}'
`, tt.expr)
			_, err := runRunbook(t, srv, src)
			require.NoError(t, err)
			require.Equal(t, 2, rec.len())
			assert.Equal(t, fmt.Sprintf(`{"got":%q}`, tt.want), string(rec.at(1).body))
		})
	}
}

// each of the five capture-failure classes produces its own message and
// halts the run without ever printing the ok line.
func TestRunCaptureFailureClasses(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		maxBodyBuffer string // "" leaves max-body-buffer unset
		expr          string
		wantContains  []string
	}{
		{name: "empty body", body: "", expr: ".v", wantContains: []string{`capture "v"`, "empty"}},
		{name: "not valid JSON", body: "not-json", expr: ".v", wantContains: []string{"not valid JSON"}},
		{
			name:          "over max-body-buffer",
			body:          fmt.Sprintf(`{"v":1,"pad":%q}`, strings.Repeat("a", 2048)),
			maxBodyBuffer: "1KiB",
			expr:          ".v",
			wantContains:  []string{"max-body-buffer"},
		},
		{
			// .missing yields null (an absent key indexes to null, not "no
			// output") — "not a scalar", not this class. Only a select() with no
			// matches emits nothing.
			name: "expression matches nothing", body: `{"v":[1,2,3]}`, expr: ".v[] | select(. == 99)",
			wantContains: []string{"matched nothing"},
		},
		{name: "expression yields an object", body: `{"v":{"a":1}}`, expr: ".v", wantContains: []string{"not a scalar"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newMuxServer(t, &recorder{}, map[string]*pathScript{
				"/x": {statuses: []int{http.StatusOK}, bodies: []string{tt.body}},
			})
			src := "calls:\n  - name: read\n    path: /x\n"
			if tt.maxBodyBuffer != "" {
				src += "    max-body-buffer: '" + tt.maxBodyBuffer + "'\n"
			}
			src += "    capture:\n      v: '" + tt.expr + "'\n"

			out, err := runRunbook(t, srv, src)
			require.Error(t, err)
			for _, want := range tt.wantContains {
				assert.ErrorContains(t, err, want)
			}
			assert.NotContains(t, out, `call "read": ok`, "a failing capture must not print the ok line")
		})
	}
}

// an oversized body does not affect a non-capturing call, and
// max-body-buffer: 0 makes even a large body capturable. The capturing-call-
// over-cap-fails case is covered by TestRunCaptureFailureClasses.
func TestRunCaptureBodySizeInteractsWithMaxBodyBuffer(t *testing.T) {
	body := fmt.Sprintf(`{"v":1,"pad":%q}`, strings.Repeat("a", 2048))

	t.Run("non-capturing call over cap still succeeds", func(t *testing.T) {
		srv := newMuxServer(t, &recorder{}, map[string]*pathScript{
			"/x": {statuses: []int{http.StatusOK}, bodies: []string{body}},
		})
		_, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /x
    max-body-buffer: '1KiB'
`)
		require.NoError(t, err)
	})

	t.Run("capturing call with max-body-buffer 0 captures from a large body", func(t *testing.T) {
		srv := newMuxServer(t, &recorder{}, map[string]*pathScript{
			"/x": {statuses: []int{http.StatusOK}, bodies: []string{body}},
		})
		out, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /x
    max-body-buffer: '0'
    capture:
      v: '.v'
`)
		require.NoError(t, err)
		assert.Contains(t, out, `call "read": ok`)
	})
}

// $${seq} reaches the server as the literal ${seq}.
func TestRunEscapedDollarBraceIsLiteral(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read": {statuses: []int{http.StatusOK}, bodies: []string{`{"seq":42}`}},
		"/next": {statuses: []int{http.StatusOK}},
	})

	_, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      seq: '.seq'
  - name: next
    path: /next
    body: '{"literal":"$${seq}"}'
`)
	require.NoError(t, err)
	require.Equal(t, 2, rec.len())
	assert.Equal(t, `{"literal":"${seq}"}`, string(rec.at(1).body))
}

// with Verbose, name=value appears on stderr for each capture.
func TestRunCaptureVerboseLogsNameValue(t *testing.T) {
	srv := newMuxServer(t, &recorder{}, map[string]*pathScript{
		"/read": {statuses: []int{http.StatusOK}, bodies: []string{`{"seq":42,"note":"ok"}`}},
	})

	rb, err := Load(strings.NewReader(`
calls:
  - name: read
    path: /read
    capture:
      seq: '.seq'
      note: '.note'
`), "")
	require.NoError(t, err)

	var stderr bytes.Buffer
	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr, Verbose: true}
	require.NoError(t, runner.Run(context.Background(), rb))

	out := stderr.String()
	assert.Contains(t, out, "seq=42\n")
	assert.Contains(t, out, "note=ok\n")
}

// the two documented jq escape hatches work end to end: tojson splices a
// whole object into a later body, and @json safely interpolates a string
// containing quotes or newlines.
func TestRunCaptureJQEscapeHatches(t *testing.T) {
	t.Run("tojson splices an object unquoted", func(t *testing.T) {
		var rec recorder
		srv := newMuxServer(t, &rec, map[string]*pathScript{
			"/read":  {statuses: []int{http.StatusOK}, bodies: []string{`{"_source":{"a":1,"b":"x"}}`}},
			"/write": {statuses: []int{http.StatusOK}},
		})

		_, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      src: '._source | tojson'
  - name: write
    path: /write
    body: '{"doc":${src}}'
`)
		require.NoError(t, err)
		require.Equal(t, 2, rec.len())

		var got map[string]any
		require.NoError(t, json.Unmarshal(rec.at(1).body, &got))
		assert.Equal(t, map[string]any{"doc": map[string]any{"a": float64(1), "b": "x"}}, got)
	})

	t.Run("@json escapes quotes and a newline", func(t *testing.T) {
		raw := "has \"quotes\" and\na newline"
		payload, err := json.Marshal(map[string]string{"reason": raw})
		require.NoError(t, err)

		var rec recorder
		srv := newMuxServer(t, &rec, map[string]*pathScript{
			"/read":  {statuses: []int{http.StatusOK}, bodies: []string{string(payload)}},
			"/write": {statuses: []int{http.StatusOK}},
		})

		_, err = runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      reason: '.reason | @json'
  - name: write
    path: /write
    body: '{"msg":${reason}}'
`)
		require.NoError(t, err)
		require.Equal(t, 2, rec.len())

		var got map[string]string
		require.NoError(t, json.Unmarshal(rec.at(1).body, &got))
		assert.Equal(t, raw, got["msg"])
	})
}

// Substitution must happen on copies, never on Call's own strings: a reused
// Runner running the same *Runbook twice must resubstitute fresh values, not
// carry over a mutated Call's resolved path.
func TestRunReusableAcrossMultipleRunsWithDifferentCaptures(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read":    {statuses: []int{http.StatusOK}, bodies: []string{`{"seq":42}`, `{"seq":99}`}},
		"/next/42": {statuses: []int{http.StatusOK}},
		"/next/99": {statuses: []int{http.StatusOK}},
	})

	rb, err := Load(strings.NewReader(`
calls:
  - name: read
    path: /read
    capture:
      seq: '.seq'
  - name: next
    path: /next/${seq}
`), "")
	require.NoError(t, err)

	runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: io.Discard}
	require.NoError(t, runner.Run(context.Background(), rb))
	require.NoError(t, runner.Run(context.Background(), rb))

	require.Equal(t, 4, rec.len())
	assert.Equal(t, []string{"/read", "/next/42", "/read", "/next/99"}, rec.paths())
}

// A capture must run against the final response the engine accepted, not
// any earlier retried one.
func TestRunCaptureUsesFinalResponseAfterRetries(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read": {statuses: []int{http.StatusServiceUnavailable, http.StatusOK}, bodies: []string{`{"seq":1}`, `{"seq":2}`}},
		"/next": {statuses: []int{http.StatusOK}},
	})

	_, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    retry: 2
    backoff-initial: '1ms'
    capture:
      seq: '.seq'
  - name: next
    path: /next
    body: '{"seq":${seq}}'
`)
	require.NoError(t, err)
	require.Equal(t, 3, rec.len())
	assert.Equal(t, `{"seq":2}`, string(rec.at(2).body), "capture runs against the final response, not the retried 503")
}

func TestRunCaptureOnToleratedCall(t *testing.T) {
	t.Run("the call itself fails: nothing captured, run continues", func(t *testing.T) {
		var rec recorder
		srv := newMuxServer(t, &rec, map[string]*pathScript{
			"/fails": {statuses: []int{http.StatusBadRequest}, bodies: []string{`{"error":"bad"}`}},
			"/next":  {statuses: []int{http.StatusOK}},
		})

		rb, err := Load(strings.NewReader(`
calls:
  - name: fails
    path: /fails
    abort-on: [400]
    continue-on-failure: true
    capture:
      seq: '.seq'
  - name: next
    path: /next
`), "")
		require.NoError(t, err)

		var stderr bytes.Buffer
		runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr, Verbose: true}
		require.NoError(t, runner.Run(context.Background(), rb))

		require.Equal(t, 2, rec.len(), "the run continues past the tolerated failure")
		assert.NotContains(t, stderr.String(), "seq=", "a failed call never reaches capture extraction")
	})

	t.Run("the call succeeds but its capture fails: tolerated, store keeps only earlier captures", func(t *testing.T) {
		var rec recorder
		srv := newMuxServer(t, &rec, map[string]*pathScript{
			"/first":  {statuses: []int{http.StatusOK}, bodies: []string{`{"a":"1"}`}},
			"/second": {statuses: []int{http.StatusOK}, bodies: []string{`{"b":{"x":1}}`}}, // .b is an object: not a scalar
			"/third":  {statuses: []int{http.StatusOK}},
		})

		rb, err := Load(strings.NewReader(`
calls:
  - name: first
    path: /first
    capture:
      a: '.a'
  - name: second
    path: /second
    continue-on-failure: true
    capture:
      b: '.b'
  - name: third
    path: /third
    body: '{"got":"${a}"}'
`), "")
		require.NoError(t, err)

		var stderr bytes.Buffer
		runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr, Verbose: true}
		require.NoError(t, runner.Run(context.Background(), rb))

		require.Equal(t, 3, rec.len(), "the run continues past the tolerated capture failure")
		assert.Equal(t, `{"got":"1"}`, string(rec.at(2).body), "the earlier capture is still in the store")
		assert.NotContains(t, stderr.String(), "b=", "the failed capture on the tolerated call was never stored")
		assert.Contains(t, stderr.String(), `call "second": failed`)
	})
}

// A captured value that would rewrite the request's target must fail
// the call, and the request it would have built must never be sent.
func TestRunCaptureSubstitutionRejectsUnsafePathValue(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read": {statuses: []int{http.StatusOK}, bodies: []string{`{"id":"../../_all"}`}},
	})

	out, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      id: '.id'
  - name: next
    path: /next/${id}
`)
	require.Error(t, err)
	assert.ErrorContains(t, err, `capture ${id} = "../../_all" cannot be substituted into path`)
	assert.ErrorContains(t, err, `contains "/"`)
	require.Equal(t, 1, rec.len(), "the /next request must never be sent")
	assert.Contains(t, out, `call "next": failed (request not sent)`)
}

// A captured value of ".." contains none of "/ ? # %", so the
// value-level check alone would let it through; a normalizing proxy would
// then collapse "/next/.." to the parent path.
func TestRunCaptureSubstitutionRejectsDotDotSegment(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read": {statuses: []int{http.StatusOK}, bodies: []string{`{"id":".."}`}},
	})

	out, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      id: '.id'
  - name: next
    path: /next/${id}
`)
	require.Error(t, err)
	assert.ErrorContains(t, err, `contains a ".." segment`)
	require.Equal(t, 1, rec.len(), "the /next request must never be sent")
	assert.Contains(t, out, `call "next": failed (request not sent)`)
}

// Neither the author's literal "." nor a captured "." is ".." alone, but
// concatenated across the template boundary they compose into one — so the
// check must run on the resolved path, not per-value.
func TestRunCaptureSubstitutionRejectsComposedDotDotSegment(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read": {statuses: []int{http.StatusOK}, bodies: []string{`{"id":"."}`}},
	})

	out, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      id: '.id'
  - name: next
    path: /next/.${id}
`)
	require.Error(t, err)
	assert.ErrorContains(t, err, `contains a ".." segment`)
	require.Equal(t, 1, rec.len(), "the /next request must never be sent")
	assert.Contains(t, out, `call "next": failed (request not sent)`)
}

// The same value that's rejected in path must still reach a body or a query
// value verbatim: only path substitution changes what is requested.
func TestRunCaptureSubstitutionAllowsUnsafeValueInBodyAndQuery(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read": {statuses: []int{http.StatusOK}, bodies: []string{`{"id":"../../_all"}`}},
		"/next": {statuses: []int{http.StatusOK}},
	})

	_, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      id: '.id'
  - name: next
    path: /next
    query:
      filter: '${id}'
    body: '{"id":"${id}"}'
`)
	require.NoError(t, err)
	require.Equal(t, 2, rec.len())
	got := rec.at(1)
	assert.Equal(t, "../../_all", got.query.Get("filter"))
	assert.Equal(t, `{"id":"../../_all"}`, string(got.body))
}

// An integer beyond float64's 2^53 exact range must round-trip exactly
// through a capture into a later call.
func TestRunCaptureLargeIntegerRoundTripsExactly(t *testing.T) {
	var rec recorder
	srv := newMuxServer(t, &rec, map[string]*pathScript{
		"/read": {statuses: []int{http.StatusOK}, bodies: []string{`{"seq":1234567890123456789}`}},
		"/next": {statuses: []int{http.StatusOK}},
	})

	_, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /read
    capture:
      seq: '.seq'
  - name: next
    path: /next
    query:
      seq: '${seq}'
`)
	require.NoError(t, err)
	require.Equal(t, 2, rec.len())
	assert.Equal(t, "1234567890123456789", rec.at(1).query.Get("seq"))
}

// A capture failure is exactly the case where the response body is the
// diagnosis, so it must be echoed like a failing-body call's is.
func TestRunCaptureFailureEchoesResponseBody(t *testing.T) {
	srv := newMuxServer(t, &recorder{}, map[string]*pathScript{
		"/x": {statuses: []int{http.StatusOK}, bodies: []string{`{"v":{"a":1}}`}},
	})

	out, err := runRunbook(t, srv, `
calls:
  - name: read
    path: /x
    capture:
      v: '.v'
`)
	require.Error(t, err)
	assert.Contains(t, out, `  {"v":{"a":1}}`, "the body that caused the capture failure must be echoed, indented")
}
