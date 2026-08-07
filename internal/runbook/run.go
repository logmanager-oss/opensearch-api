package runbook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// Runner executes a loaded Runbook sequentially against Endpoint. Client and
// Endpoint must be set; a nil Progress or Warn is defaulted to io.Discard.
type Runner struct {
	Client   *http.Client
	Endpoint string
	Progress io.Writer // per-call progress; CLI passes stderr
	Verbose  bool      // per-attempt retry lines
	Warn     io.Writer // engine warnings (retry.WithWarn)
}

// outcome is the terminal state of one entry call within a run.
type outcome int

const (
	succeeded outcome = iota + 1
	failed
	skipped
)

// Run executes rb.Entries in document order, tracking each entry's outcome.
// A failed call is logged and does not stop the run unless it has
// stop-on-failure, which cascades a skip to every remaining entry. A call
// depending on a not-yet-succeeded prerequisite is skipped instead of run,
// and that skip cascades to its own dependents in turn. Context cancellation
// stops the run immediately, returning the canceling call's error unjoined
// so errors.Is(err, context.Canceled) keeps working for exit-code mapping.
// Otherwise the result is nil when every entry succeeded, or an errors.Join
// of the per-call failures.
func (r *Runner) Run(ctx context.Context, rb *Runbook) error {
	if r.Progress == nil {
		r.Progress = io.Discard
	}
	if r.Warn == nil {
		r.Warn = io.Discard
	}

	outcomes := make([]outcome, len(rb.Calls))
	errs := make([]error, 0, len(rb.Entries))

	for i, idx := range rb.Entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		call := &rb.Calls[idx]
		if dep, ok := unsucceededDep(rb, call, outcomes); ok {
			outcomes[idx] = skipped
			_, _ = fmt.Fprintf(r.Progress, "call %q: skipped (needs %s)\n", call.Name, dep)
			continue
		}

		err := r.runCall(ctx, rb, idx, 0)
		if err == nil {
			outcomes[idx] = succeeded
			continue
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		// The run was interrupted while (or right after) the call failed on
		// its own; join so the cancellation stays errors.Is-matchable for the
		// 130 exit path.
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}

		outcomes[idx] = failed
		errs = append(errs, err)

		if call.StopOnFailure {
			r.skipRemaining(rb, outcomes, rb.Entries[i+1:])
			break
		}
	}

	r.printSummary(rb, outcomes)

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// skipRemaining marks every given entry index skipped, logging each; used for
// the stop-on-failure cascade.
func (r *Runner) skipRemaining(rb *Runbook, outcomes []outcome, entries []int) {
	for _, idx := range entries {
		outcomes[idx] = skipped
		_, _ = fmt.Fprintf(r.Progress, "call %q: skipped (stop-on-failure)\n", rb.Calls[idx].Name)
	}
}

// printSummary tallies the entry outcomes and writes the final summary line.
func (r *Runner) printSummary(rb *Runbook, outcomes []outcome) {
	var nSucceeded, nFailed, nSkipped int
	for _, idx := range rb.Entries {
		switch outcomes[idx] {
		case succeeded:
			nSucceeded++
		case failed:
			nFailed++
		case skipped:
			nSkipped++
		}
	}
	_, _ = fmt.Fprintf(r.Progress, "run: %d succeeded, %d failed, %d skipped\n", nSucceeded, nFailed, nSkipped)
}

// unsucceededDep returns the first of call's depends-on prerequisites whose
// outcome is not succeeded, if any; validated at load time, every dependency
// names an earlier entry call so its outcome is always already decided.
func unsucceededDep(rb *Runbook, call *Call, outcomes []outcome) (string, bool) {
	for _, dep := range call.DependsOn {
		if outcomes[rb.byName[dep]] != succeeded {
			return dep, true
		}
	}
	return "", false
}

// runCall builds and executes the request for rb.Calls[idx], driving it
// through a retry.Engine, logging its outcome to r.Progress, and returning a
// call-qualified error (nil on success) so sentinels stay errors.Is-matchable.
// depth is 0 for an entry call and depth+1 for each nested verify-with check;
// it controls both the two-space indent and the "call"/"check" label of every
// line runCall prints.
func (r *Runner) runCall(ctx context.Context, rb *Runbook, idx, depth int) error {
	call := &rb.Calls[idx]
	prefix, label := indent(depth), callLabel(depth)

	req, err := osclient.BuildRequest(r.Endpoint, osclient.RequestSpec{
		Method:  call.Method,
		Path:    call.Path,
		Body:    call.Body,
		HasBody: call.HasBody,
		Query:   call.Query,
		Headers: call.Headers,
	})
	if err != nil {
		_, _ = fmt.Fprintf(r.Progress, "%s%s %q: failed (%v)\n", prefix, label, call.Name, err)
		return fmt.Errorf("%s %q: %w", label, call.Name, err)
	}

	if call.VerifyWith != "" {
		_, _ = fmt.Fprintf(r.Progress, "%s%s %q: %s %s\n", prefix, label, call.Name, call.Method, call.Path)
	}

	attempts := 0
	attempt := func(ctx context.Context) (*http.Response, error) {
		attempts++
		c := req.Clone(ctx)
		if req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("preparing request body: %w", err)
			}
			c.Body = b
		}
		return r.Client.Do(c)
	}

	opts := []retry.Option{
		retry.WithRetryWhen(call.RetryWhen),
		retry.WithSuccessWhen(call.SuccessWhen),
		retry.WithWarn(r.Warn),
	}
	if r.Verbose {
		opts = append(opts, retry.WithOnRetry(r.verboseHook(call.Name, prefix, label)))
	}
	if call.VerifyWith != "" {
		checkIdx := rb.byName[call.VerifyWith]
		// The check is a separate request on its own connection, so the outer
		// response's body being left open (unclosed) by retry.Engine.Do while
		// this runs is harmless.
		opts = append(opts, retry.WithSuccessCheck(func(ctx context.Context) (bool, error) {
			return r.runVerifyCheck(ctx, rb, checkIdx, depth)
		}))
	}

	// The engine consumes only the compiled predicates passed as options;
	// clear the raw strings so they cannot become a second source of truth.
	// Load validation makes success-when and verify-with mutually exclusive,
	// so New's option check cannot fire for a loaded runbook.
	engineCfg := call.Retry
	engineCfg.RetryWhen, engineCfg.SuccessWhen = "", ""
	engine, err := retry.New(engineCfg, opts...)
	if err != nil {
		_, _ = fmt.Fprintf(r.Progress, "%s%s %q: failed (%v)\n", prefix, label, call.Name, err)
		return fmt.Errorf("%s %q: building retry engine: %w", label, call.Name, err)
	}

	resp, doErr := engine.Do(ctx, attempt)

	status := 0
	if resp != nil {
		status = resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	if doErr != nil {
		_, _ = fmt.Fprintf(r.Progress, "%s%s %q: failed (%s)\n", prefix, label, call.Name, outcomeDetail(status, attempts, doErr))
		return fmt.Errorf("%s %q: %w", label, call.Name, doErr)
	}

	_, _ = fmt.Fprintf(r.Progress, "%s%s %q: ok (%s)\n", prefix, label, call.Name, outcomeDetail(status, attempts, nil))
	return nil
}

// runVerifyCheck runs the nested verify-with check at rb.Calls[checkIdx] one
// level deeper than depth and maps its result to the (bool, error) shape
// retry.WithSuccessCheck expects: success maps to (true, nil); exhausted
// retries map to (false, nil) — "not yet", so the outer attempt just retries
// per its own policy; every other failure (cancellation, the check's own
// abort-on, a deterministic request-build error) maps to (false, err) so it
// propagates out of the outer Do immediately instead of being retried forever.
func (r *Runner) runVerifyCheck(ctx context.Context, rb *Runbook, checkIdx, depth int) (bool, error) {
	err := r.runCall(ctx, rb, checkIdx, depth+1)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, retry.ErrRetriesExhausted) {
		return false, nil
	}
	return false, err
}

// verboseHook returns an OnRetry hook that reports each failed attempt of the
// named call to r.Progress, mirroring the CLI's verbose request output.
func (r *Runner) verboseHook(name, prefix, label string) func(retry.RetryInfo) {
	return func(info retry.RetryInfo) {
		if info.Err != nil {
			_, _ = fmt.Fprintf(r.Progress, "%s%s %q: attempt %d failed: %v; retrying in %s\n",
				prefix, label, name, info.Attempt, info.Err, info.Delay)
			return
		}
		if info.Reason != "" {
			_, _ = fmt.Fprintf(r.Progress, "%s%s %q: attempt %d: status %d (%s); retrying in %s\n",
				prefix, label, name, info.Attempt, info.Status, info.Reason, info.Delay)
			return
		}
		_, _ = fmt.Fprintf(r.Progress, "%s%s %q: attempt %d: status %d; retrying in %s\n",
			prefix, label, name, info.Attempt, info.Status, info.Delay)
	}
}

// indent returns two spaces per depth, offsetting nested check lines under
// the call they verify.
func indent(depth int) string {
	return strings.Repeat("  ", depth)
}

// callLabel returns "call" for an entry call (depth 0) or "check" for a
// nested verify-with invocation (depth > 0).
func callLabel(depth int) string {
	if depth == 0 {
		return "call"
	}
	return "check"
}

// outcomeDetail formats the parenthetical detail of a call's outcome line:
// "status <code>, terminal" for an aborted status, "status <code>, N
// attempts" otherwise, or the bare attempt count when no response was ever
// received (a transport error). A terminal error with no status can only be
// a propagated verify-with check abort — the call's own abort-on always has
// its response's status — so it is labeled as the check's, not the call's.
func outcomeDetail(status, attempts int, err error) string {
	descriptor := attemptWord(attempts)
	if errors.Is(err, retry.ErrTerminalStatus) {
		if status == 0 {
			return "check terminal"
		}
		descriptor = "terminal"
	}
	if status == 0 {
		return descriptor
	}
	return fmt.Sprintf("status %d, %s", status, descriptor)
}

// attemptWord formats an attempt count with correct pluralization.
func attemptWord(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}
