// Package retry provides a configurable, context-aware retry engine for HTTP
// requests: it classifies each outcome and backs off between attempts until
// success, a terminal status, or attempt exhaustion.
package retry

import (
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

// classify decides the outcome of an attempt from its status and transport
// error. Precedence: success is checked before terminal, so a 2xx (or a code in
// SuccessStatus) that also appears in TerminalStatus resolves to Success.
// Terminal is checked before the retry whitelist, so a code present in both
// TerminalStatus and RetryStatus resolves to Terminal.
//
//nolint:gocritic // hugeParam: RetryConfig passed by value by design (small, immutable).
func classify(cfg config.RetryConfig, status int, transportErr error) Outcome {
	if transportErr != nil {
		return Retry
	}
	c := &cfg
	if isSuccess(c, status) {
		return Success
	}
	if slices.Contains(c.TerminalStatus, status) {
		return Terminal
	}
	if len(c.RetryStatus) > 0 {
		if slices.Contains(c.RetryStatus, status) {
			return Retry
		}
		return Terminal
	}
	return Retry
}

func isSuccess(cfg *config.RetryConfig, status int) bool {
	if len(cfg.SuccessStatus) > 0 {
		return slices.Contains(cfg.SuccessStatus, status)
	}
	return status >= statusOK && status < statusMultipleChoice
}
