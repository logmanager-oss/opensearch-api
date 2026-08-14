package runbook

import (
	"bytes"
	"context"
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

// capture records requests in arrival order, guarded for concurrent handlers.
type capture struct {
	mu   sync.Mutex
	reqs []recordedCall
}

func (c *capture) add(r recordedCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
}

func (c *capture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func (c *capture) at(i int) recordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs[i]
}

func (c *capture) paths() []string {
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
func newMuxServer(t *testing.T, rec *capture, scripts map[string]*pathScript) *httptest.Server {
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
	var rec capture
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

	// Behavior 1: calls execute in document order.
	assert.Equal(t, []string{"/first", "/second", "/third"}, rec.paths())

	// Behavior 2: all-success summary.
	assert.Contains(t, stderr.String(), `run: 3 succeeded`)
}

func TestRunHaltsOnFailure(t *testing.T) {
	var rec capture
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

	// Behavior 3: a failing call halts the run; later calls receive no requests.
	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrTerminalStatus, "Run returns the halting call's error")
	assert.ErrorContains(t, err, `call "second"`, "wrapped with the call name")
	assert.Equal(t, []string{"/first", "/second"}, rec.paths(), "third and fourth must not run")
	assert.Contains(t, stderr.String(), "run: 1 succeeded, 1 failed (halted), 2 not run")
}

func TestRunContinueOnFailure(t *testing.T) {
	var rec capture
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

	// Behavior 4: continue-on-failure lets the run continue and return nil.
	require.NoError(t, err)
	assert.Equal(t, []string{"/first", "/second", "/third"}, rec.paths())
	out := stderr.String()
	// Anchored to the outcome line: the summary alone also contains "(tolerated)".
	assert.Contains(t, out, `call "second": failed (status 400, terminal, 1 attempt) (tolerated)`)
	assert.Contains(t, out, "run: 2 succeeded, 1 failed (tolerated)")
}

func TestRunFailingBodyEchoed(t *testing.T) {
	var rec capture
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
	// Behavior 5: failing bodies (halting and tolerated) are echoed, indented;
	// a successful call's body is not.
	assert.Contains(t, out, `  {"error":"boom"}`)
	assert.Contains(t, out, `  {"error":"tolerated-boom"}`)
	assert.NotContains(t, out, `{"ack":true}`)
}

func TestRunFailingBodyTruncation(t *testing.T) {
	bigBody := strings.Repeat("a", 2048) // > 1KiB
	var rec capture
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
	// Behavior 6: over-cap body is truncated to exactly 1KiB with a trailing
	// marker; the same call with max-body-buffer: 0 (the LimitReader(_, 0)
	// trap) prints the full 2KiB body untruncated.
	assert.Contains(t, out, strings.Repeat("a", 1024)+"\n  … (truncated at 1KiB)",
		"truncated content must stop at exactly 1KiB, not 1KiB+1")
	assert.Contains(t, out, bigBody, "max-body-buffer: 0 means unlimited")
	assert.Equal(t, 1, strings.Count(out, "truncated at"), "only the capped call should report truncation")
}

func TestRunSuccessWhenExhaustsWithReason(t *testing.T) {
	var rec capture
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
	var rec capture
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

	// Behavior 9: success-when accepting a non-2xx succeeds without retrying.
	require.NoError(t, err)
	assert.Equal(t, 1, rec.len())
	assert.Contains(t, stderr.String(), `call "maybe_missing": ok (status 404, 1 attempt)`)
}

func TestRunContextCanceledMidRun(t *testing.T) {
	var rec capture
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

	// Behavior 10: a canceled context returns promptly with an error matching
	// context.Canceled, the summary names the interrupted call and the
	// not-run count, and no further calls were attempted.
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"/first", "/wait_green"}, rec.paths())
	assert.Contains(t, stderr.String(), `run: 1 succeeded, interrupted during call "wait_green", 2 not run`)
}

func TestRunRequestDetailsReachServer(t *testing.T) {
	var rec capture
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

	// Behavior 11: body, headers and query params from the YAML reach the server.
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
		var rec capture
		return newMuxServer(t, &rec, map[string]*pathScript{
			"/flaky": {statuses: []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusOK}},
		})
	}

	t.Run("quiet", func(t *testing.T) {
		srv := newFlakyServer(t)
		var stderr bytes.Buffer
		runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr}
		require.NoError(t, runner.Run(context.Background(), rb))

		// Behavior 12 (attempt count): a call succeeding on its 3rd attempt
		// reports "3 attempts".
		out := stderr.String()
		assert.Contains(t, out, `call "flaky": ok (status 200, 3 attempts)`)
		// Behavior 12 (verbose gating): no per-attempt retry lines when quiet.
		assert.NotContains(t, out, "retrying in")
	})

	t.Run("verbose", func(t *testing.T) {
		srv := newFlakyServer(t)
		var stderr bytes.Buffer
		runner := &Runner{Client: http.DefaultClient, Endpoint: srv.URL, Stderr: &stderr, Verbose: true}
		require.NoError(t, runner.Run(context.Background(), rb))

		// Behavior 12 (verbose gating): per-attempt retry lines appear, prefixed
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
		var rec capture
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
		srv := newMuxServer(t, &capture{}, map[string]*pathScript{"/x": {statuses: []int{http.StatusOK}}})
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
	var rec capture
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
	var rec capture
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
	var rec capture
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
	srv := newMuxServer(t, &capture{}, map[string]*pathScript{"/x": {statuses: []int{http.StatusOK}}})
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
	srv := newMuxServer(t, &capture{}, map[string]*pathScript{"/fine": {statuses: []int{http.StatusOK}}})

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
	var rec capture
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
	var rec capture
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
	var rec capture
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
