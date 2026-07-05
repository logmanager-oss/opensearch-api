package cli

import (
	"fmt"
	"io"

	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// verboseHook returns an OnRetry hook that reports each failed attempt to w, or
// nil when verbose is off. This is the deliberate exception to the internal
// logging convention: osapi is a human-facing CLI whose diagnostics are plain
// text on stderr, kept separate from the response body on stdout.
func verboseHook(w io.Writer, verbose bool) func(retry.RetryInfo) {
	if !verbose {
		return nil
	}
	return func(info retry.RetryInfo) {
		if info.Err != nil {
			_, _ = fmt.Fprintf(w, "attempt %d failed: %v; retrying in %s\n",
				info.Attempt, info.Err, info.Delay)
			return
		}
		_, _ = fmt.Fprintf(w, "attempt %d: status %d; retrying in %s\n",
			info.Attempt, info.Status, info.Delay)
	}
}
