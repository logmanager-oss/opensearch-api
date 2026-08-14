package runbook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// Runner executes a loaded Runbook sequentially against a live OpenSearch
// endpoint.
type Runner struct {
	// Client must not set a Timeout: its timeout errors are indistinguishable
	// from context cancellation; deadlines belong on the context passed to Run.
	Client   *http.Client
	Endpoint string
	Stderr   io.Writer
	Verbose  bool
}

// Run executes rb.Calls in document order, halting on the first failure not
// tolerated via continue-on-failure, and always writes a summary line to
// Stderr. The returned error has already been reported there; callers must
// not print it again.
func (r *Runner) Run(ctx context.Context, rb *Runbook) error {
	// store is local to Run, not a Runner field, so repeated Run calls don't
	// leak captures between runs.
	store := make(map[string]string)
	var succeeded, tolerated int
	for idx := range rb.Calls {
		call := &rb.Calls[idx]
		err := r.runCall(ctx, call, store)
		if err == nil {
			succeeded++
			continue
		}

		notRun := len(rb.Calls) - idx - 1
		if isContextErr(ctx, err) {
			r.writeCanceledSummary(succeeded, tolerated, call.Name, notRun)
			return err
		}
		if call.ContinueOnFailure {
			tolerated++
			continue
		}

		r.writeHaltSummary(succeeded, tolerated, notRun)
		return err
	}

	r.writeCompletedSummary(succeeded, tolerated)
	return nil
}

// attemptResult bundles a retry loop's response and error with the attempt
// count and raw transport error retry.Do doesn't return.
type attemptResult struct {
	attempts     int
	resp         *http.Response
	doErr        error
	transportErr error
}

// runCall executes call under its retry policy, substituting store's
// captured values from earlier calls into a request-ready spec.
func (r *Runner) runCall(ctx context.Context, call *Call, store map[string]string) error {
	spec, err := substituteCall(call, store)
	if err != nil {
		// Named here: nothing was sent, so there is no status, attempts or body.
		_, _ = fmt.Fprintf(r.Stderr, "call %q: failed (request not sent)%s: %v\n",
			call.Name, toleratedSuffix(call), err)
		return fmt.Errorf("call %q: %w", call.Name, err)
	}

	req, err := osclient.BuildRequest(r.Endpoint, spec)
	if err != nil {
		_, _ = fmt.Fprintf(r.Stderr, "call %q: failed (request not sent)%s: %v\n",
			call.Name, toleratedSuffix(call), err)
		return fmt.Errorf("call %q: building request: %w", call.Name, err)
	}

	opts := []retry.Option{
		retry.WithRetryWhen(call.RetryWhen),
		retry.WithSuccessWhen(call.SuccessWhen),
		retry.WithWarn(r.Stderr),
	}
	if r.Verbose {
		opts = append(opts, retry.WithOnRetry(callVerboseHook(r.Stderr, call.Name)))
	}
	engine := retry.New(call.Retry, opts...)

	// transportErr keeps the last attempt's raw error: doErr wraps it behind
	// an "after N attempts" prefix that duplicates the outcome line's count.
	var attempts int
	var transportErr error
	resp, doErr := engine.Do(ctx, func(attemptCtx context.Context) (*http.Response, error) { //nolint:bodyclose // reportOutcome (via drainDiscard/echoBody) closes resp on every path
		attempts++
		cloned := req.Clone(attemptCtx)
		if req.GetBody != nil {
			body, bodyErr := req.GetBody()
			if bodyErr != nil {
				transportErr = fmt.Errorf("preparing request body: %w", bodyErr)
				return nil, transportErr
			}
			cloned.Body = body
		}
		attemptResp, attemptErr := r.Client.Do(cloned)
		transportErr = attemptErr
		return attemptResp, attemptErr
	})

	return r.reportOutcome(ctx, call, attemptResult{attempts: attempts, resp: resp, doErr: doErr, transportErr: transportErr}, store)
}

// substituteCall builds a fresh RequestSpec by substituting ${name} refs
// from store into copies of call's path, query, headers and body, so a
// reused Runner never mutates call. Only path substitution can fail: a
// captured value there could redirect the request or inject query
// parameters.
func substituteCall(call *Call, store map[string]string) (osclient.RequestSpec, error) {
	path, err := substitutePath(call.Path, store)
	if err != nil {
		return osclient.RequestSpec{}, err
	}

	spec := osclient.RequestSpec{Method: call.Method, Path: path, HasBody: call.HasBody}

	if len(call.Query) > 0 {
		spec.Query = make(map[string]string, len(call.Query))
		for k, v := range call.Query {
			spec.Query[k] = substitute(v, store)
		}
	}

	if len(call.Headers) > 0 {
		spec.Headers = make(http.Header, len(call.Headers))
		for k, values := range call.Headers {
			substituted := make([]string, len(values))
			for i, v := range values {
				substituted[i] = substitute(v, store)
			}
			spec.Headers[k] = substituted
		}
	}

	if call.HasBody {
		if bytes.Contains(call.Body, []byte("${")) {
			spec.Body = []byte(substitute(string(call.Body), store))
		} else {
			// BuildRequest only reads spec.Body, so an untemplated body can
			// share call.Body's backing array without copying.
			spec.Body = call.Body
		}
	}

	return spec, nil
}

// reportOutcome writes the outcome line and always drains/closes ar.resp
// exactly once. A context error is left unreported: only Run's summary
// names it.
func (r *Runner) reportOutcome(ctx context.Context, call *Call, ar attemptResult, store map[string]string) error {
	if ar.doErr == nil {
		return r.reportSuccess(ctx, call, ar, store)
	}

	if isContextErr(ctx, ar.doErr) {
		drainDiscard(ar.resp)
		return fmt.Errorf("call %q: %w", call.Name, ar.doErr)
	}

	r.writeFailure(call, ar)
	return fmt.Errorf("call %q: %w", call.Name, ar.doErr)
}

// reportSuccess writes the ok line, or — if a capture can't be extracted —
// a failure line echoing the body, the diagnosis for "matched nothing" and
// "not a scalar". Capture runs first, so ok is never printed before a
// capture failure.
func (r *Runner) reportSuccess(ctx context.Context, call *Call, ar attemptResult, store map[string]string) error {
	var body []byte
	var captureErr error
	if len(call.Capture) == 0 {
		drainDiscard(ar.resp)
	} else if body, captureErr = r.captureFromResponse(ctx, call, ar.resp, store); captureErr != nil {
		if isContextErr(ctx, captureErr) {
			return fmt.Errorf("call %q: %w", call.Name, captureErr)
		}
		_, _ = fmt.Fprintf(r.Stderr, "call %q: failed (status %d, %d %s)%s: %v\n",
			call.Name, ar.resp.StatusCode, ar.attempts, attemptWord(ar.attempts), toleratedSuffix(call), captureErr)
		writeIndented(r.Stderr, body)
		return fmt.Errorf("call %q: %w", call.Name, captureErr)
	}

	_, _ = fmt.Fprintf(r.Stderr, "call %q: ok (status %d, %d %s)\n",
		call.Name, ar.resp.StatusCode, ar.attempts, attemptWord(ar.attempts))
	return nil
}

// captureFromResponse reads resp's body via readBounded, extracts every
// capture in document order into store, and logs each as name=value when
// Verbose. If the call also has a predicate, the body was already buffered
// once by retry.go's bufferBody — this is a second, bounded copy of it. The
// bytes are returned alongside any error so reportSuccess can echo them on a
// capture failure without re-reading the now-closed body.
func (r *Runner) captureFromResponse(ctx context.Context, call *Call, resp *http.Response, store map[string]string) ([]byte, error) {
	data, truncated, err := readBounded(resp.Body, call.Retry.MaxBodyBuffer)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if err := extractCaptures(ctx, call.Capture, data, truncated, call.Retry.MaxBodyBuffer, store); err != nil {
		return data, err
	}

	if r.Verbose {
		for _, c := range call.Capture {
			_, _ = fmt.Fprintf(r.Stderr, "%s=%s\n", c.Name, store[c.Name])
		}
	}
	return nil, nil
}

func (r *Runner) writeFailure(call *Call, ar attemptResult) {
	suffix := toleratedSuffix(call)

	// resp is nil on both a transport error and a failed body read; labeling
	// the latter "transport error" would point diagnosis at the network.
	if ar.resp == nil && ar.transportErr != nil {
		_, _ = fmt.Fprintf(r.Stderr, "call %q: failed (transport error, %d %s)%s: %v\n",
			call.Name, ar.attempts, attemptWord(ar.attempts), suffix, ar.transportErr)
		return
	}
	if ar.resp == nil {
		_, _ = fmt.Fprintf(r.Stderr, "call %q: failed (%s, %d %s)%s\n",
			call.Name, failureDetail(ar.doErr), ar.attempts, attemptWord(ar.attempts), suffix)
		return
	}

	_, _ = fmt.Fprintf(r.Stderr, "call %q: failed (status %d, %s, %d %s)%s\n",
		call.Name, ar.resp.StatusCode, failureDetail(ar.doErr), ar.attempts, attemptWord(ar.attempts), suffix)
	r.echoBody(ar.resp, call.Retry.MaxBodyBuffer)
}

func toleratedSuffix(call *Call) string {
	if call.ContinueOnFailure {
		return " (tolerated)"
	}
	return ""
}

// failureDetail extracts why the call failed: "terminal" for an abort-on
// status, or the engine's "retries exhausted[: reason]" text.
func failureDetail(doErr error) string {
	if errors.Is(doErr, retry.ErrTerminalStatus) {
		return "terminal"
	}
	msg := doErr.Error()
	if i := strings.Index(msg, retry.ErrRetriesExhausted.Error()); i >= 0 {
		return msg[i:]
	}
	return msg
}

func (r *Runner) echoBody(resp *http.Response, maxBuffer int64) {
	data, truncated, err := readBounded(resp.Body, maxBuffer)
	_ = resp.Body.Close()

	writeIndented(r.Stderr, data)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(r.Stderr, "  … (body read failed: %v)\n", err)
	case truncated:
		_, _ = fmt.Fprintf(r.Stderr, "  … (truncated at %s)\n", config.FormatSize(maxBuffer))
	}
}

// readBounded reads up to maxBuffer bytes, reporting truncation. <=0 or
// MaxInt64 means unlimited (mirroring retry.bufferBody): LimitReader(body, 0)
// would read nothing, and at MaxInt64 the +1 overflows negative.
func readBounded(body io.Reader, maxBuffer int64) (data []byte, truncated bool, err error) {
	if maxBuffer <= 0 || maxBuffer == math.MaxInt64 {
		data, err = io.ReadAll(body)
		return data, false, err
	}
	data, err = io.ReadAll(io.LimitReader(body, maxBuffer+1))
	if int64(len(data)) > maxBuffer {
		return data[:maxBuffer], true, err
	}
	return data, false, err
}

// writeIndented writes data two-space indented per line, stripping control
// characters so the untrusted body (bare CR, ANSI escapes) cannot overwrite
// the progress lines above it.
func writeIndented(w io.Writer, data []byte) {
	text := bytes.TrimRight(data, "\n")
	if len(text) == 0 {
		return
	}
	for line := range bytes.SplitSeq(text, []byte("\n")) {
		_, _ = fmt.Fprintf(w, "  %s\n", stripControl(line))
	}
}

// stripControl removes C0/C1 control characters and DEL, keeping tab.
func stripControl(line []byte) []byte {
	return bytes.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, line)
}

// drainDiscard drains and closes resp's body (nil-safe) for connection reuse.
func drainDiscard(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func attemptWord(n int) string {
	if n == 1 {
		return "attempt"
	}
	return "attempts"
}

// isContextErr reports whether err reflects ctx's own cancellation. Both
// sides matter: a terminal failure racing a SIGTERM is not a context error,
// and a deadline error manufactured below ctx (an http.Client Timeout)
// leaves ctx.Err() nil — both stay ordinary failures.
func isContextErr(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

// callVerboseHook mirrors cli's unexported verboseHook (sharing it would
// invert the dependency), prefixed with the call name.
func callVerboseHook(w io.Writer, name string) func(retry.RetryInfo) {
	return func(info retry.RetryInfo) {
		if info.Err != nil {
			_, _ = fmt.Fprintf(w, "call %q: attempt %d failed: %v; retrying in %s\n",
				name, info.Attempt, info.Err, info.Delay)
			return
		}
		if info.Reason != "" {
			_, _ = fmt.Fprintf(w, "call %q: attempt %d: status %d (%s); retrying in %s\n",
				name, info.Attempt, info.Status, info.Reason, info.Delay)
			return
		}
		_, _ = fmt.Fprintf(w, "call %q: attempt %d: status %d; retrying in %s\n",
			name, info.Attempt, info.Status, info.Delay)
	}
}

func (r *Runner) writeCompletedSummary(succeeded, tolerated int) {
	if tolerated == 0 {
		_, _ = fmt.Fprintf(r.Stderr, "run: %d succeeded\n", succeeded)
		return
	}
	_, _ = fmt.Fprintf(r.Stderr, "run: %d succeeded, %d failed (tolerated)\n", succeeded, tolerated)
}

// "1 failed (halted)", not a bare "1 failed": next to a tolerated count, two
// adjacent "failed" clauses would read as one total.
func (r *Runner) writeHaltSummary(succeeded, tolerated, notRun int) {
	if tolerated > 0 {
		_, _ = fmt.Fprintf(r.Stderr, "run: %d succeeded, %d failed (tolerated), 1 failed (halted), %d not run\n",
			succeeded, tolerated, notRun)
		return
	}
	_, _ = fmt.Fprintf(r.Stderr, "run: %d succeeded, 1 failed (halted), %d not run\n", succeeded, notRun)
}

// After a SIGTERM this line is the only record of the run, so it accounts
// for tolerated failures too.
func (r *Runner) writeCanceledSummary(succeeded, tolerated int, name string, notRun int) {
	if tolerated > 0 {
		_, _ = fmt.Fprintf(r.Stderr, "run: %d succeeded, %d failed (tolerated), interrupted during call %q, %d not run\n",
			succeeded, tolerated, name, notRun)
		return
	}
	_, _ = fmt.Fprintf(r.Stderr, "run: %d succeeded, interrupted during call %q, %d not run\n", succeeded, name, notRun)
}
