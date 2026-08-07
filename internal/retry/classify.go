// Package retry provides a configurable, context-aware retry engine for HTTP
// requests: it classifies each outcome — optionally guided by jq body
// predicates — and backs off between attempts until success, a terminal
// status, or attempt exhaustion.
package retry

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

// Outcome is the classification of a single request attempt.
type Outcome int

const (
	// Success means the response is acceptable and should be returned.
	Success Outcome = iota + 1
	// Retry means the attempt should be retried after a backoff.
	Retry
	// Terminal means the attempt failed permanently and must not be retried.
	Terminal
)

func (o Outcome) String() string {
	switch o {
	case Success:
		return "Success"
	case Retry:
		return "Retry"
	case Terminal:
		return "Terminal"
	default:
		return "Outcome(" + strconv.Itoa(int(o)) + ")"
	}
}

const (
	statusOK             = 200
	statusMultipleChoice = 300
)

// classify decides the outcome of an attempt and, when not the plain
// status-based default, a short reason describing why. Precedence: a
// transport error always retries; a non-2xx status listed in AbortOn is
// terminal (abort-on wins over predicates and the success check, which are
// not invoked); a truthy RetryWhen retries (a failure indicator beats a
// success gate, and the success check is not invoked); a configured
// SuccessWhen decides success or retry on its own truthiness, regardless of
// status; a configured successCheck is consulted only on a 2xx response and
// decides success or retry on its own result, a non-2xx retries without
// invoking it; otherwise any 2xx is success and everything else retries —
// unchanged from the no-predicate behaviour. SuccessWhen and successCheck
// never coexist (New rejects the combination) — no ordering between them is
// defined here.
// A non-nil error is either a context error from predicate evaluation or any
// error returned by successCheck, both of which the loop must propagate
// instead of acting on the outcome.
func (e *Engine) classify(ctx context.Context, status int, body []byte, overflowed bool, transportErr error) (Outcome, string, error) {
	if transportErr != nil {
		return Retry, "", nil
	}
	is2xx := status >= statusOK && status < statusMultipleChoice
	if !is2xx && slices.Contains(e.cfg.AbortOn, status) {
		return Terminal, "", nil
	}
	if e.hasPredicates() {
		input, ok := e.decodeBody(body, overflowed)
		matched, err := e.matches(ctx, e.retryWhen, input, ok)
		if err != nil {
			return Retry, "", err
		}
		if matched {
			return Retry, "--retry-when matched", nil
		}
		if e.successWhen != nil {
			matched, err := e.matches(ctx, e.successWhen, input, ok)
			if err != nil {
				return Retry, "", err
			}
			if matched {
				return Success, "", nil
			}
			return Retry, "--success-when not satisfied", nil
		}
	}
	if e.successCheck != nil && is2xx {
		ok, err := e.successCheck(ctx)
		if err != nil {
			return Retry, "", err
		}
		if !ok {
			return Retry, "success check failed", nil
		}
		return Success, "", nil
	}
	if is2xx {
		return Success, "", nil
	}
	return Retry, "", nil
}

// decodeBody decodes body as JSON for predicate evaluation, warning once per
// attempt and reporting ok=false when it cannot: an overflowed or empty body,
// or one that fails to parse.
func (e *Engine) decodeBody(body []byte, overflowed bool) (input any, ok bool) {
	if overflowed {
		_, _ = fmt.Fprintf(e.warn, "response body exceeds --max-body-buffer (%s); --retry-when/--success-when not evaluated\n",
			config.FormatSize(e.cfg.MaxBodyBuffer))
		return nil, false
	}
	if len(body) == 0 {
		_, _ = fmt.Fprintln(e.warn, "empty response body; --retry-when/--success-when not evaluated")
		return nil, false
	}
	if err := json.Unmarshal(body, &input); err != nil {
		_, _ = fmt.Fprintf(e.warn, "response body is not valid JSON; --retry-when/--success-when not evaluated: %v\n", err)
		return nil, false
	}
	return input, true
}

// matches reports whether p is truthy against input. A context error from
// evaluation (a non-terminating expression cut short by RunWithContext)
// propagates without warning so the loop treats it like any other context
// error; other evaluation errors warn and count as not matched.
func (e *Engine) matches(ctx context.Context, p *Predicate, input any, ok bool) (bool, error) {
	if p == nil || !ok {
		return false, nil
	}
	matched, err := p.match(ctx, input)
	if err == nil {
		return matched, nil
	}
	if ctxErr := contextError(ctx, err); ctxErr != nil {
		return false, ctxErr
	}
	_, _ = fmt.Fprintf(e.warn, "evaluating predicate %q: %v; treating it as not matched\n", p.String(), err)
	return false, nil
}
