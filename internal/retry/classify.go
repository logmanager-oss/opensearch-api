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
// error: any 2xx is Success; a status listed in AbortOn is Terminal (stop
// retrying); everything else (including transport errors) is Retry.
//
//nolint:gocritic // hugeParam: RetryConfig passed by value by design (small, immutable).
func classify(cfg config.RetryConfig, status int, transportErr error) Outcome {
	if transportErr != nil {
		return Retry
	}
	if status >= statusOK && status < statusMultipleChoice {
		return Success
	}
	if slices.Contains(cfg.AbortOn, status) {
		return Terminal
	}
	return Retry
}
