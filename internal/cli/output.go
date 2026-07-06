package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/logmanager-oss/opensearch-api/internal/apispec"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// printBodySkeleton writes a JSON request-body scaffold for path/method to out.
// stdout stays valid JSON ("{}" when there is no field scaffold) so it pipes to
// jq; a note goes to errw. Array and free-form bodies have no named top-level
// fields, so they fall here too. It never sends a request.
func printBodySkeleton(out, errw io.Writer, path, method string) error {
	if skel, ok := apispec.BodySkeleton(path, method); ok {
		_, err := fmt.Fprintln(out, skel)
		return err
	}
	if _, err := fmt.Fprintln(out, "{}"); err != nil {
		return err
	}
	ref := path
	if tmpl, ok := apispec.MatchTemplate(path); ok {
		ref = tmpl
	}
	_, _ = fmt.Fprintf(errw, "no top-level field template for %s %s\n", strings.ToUpper(method), ref)
	return nil
}

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
