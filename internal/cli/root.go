// Package cli builds the osapi command: a resilient client for OpenSearch REST
// endpoints with configurable retry. The root command itself sends a single
// request. There is no separate "request" subcommand. The "run" subcommand
// instead runs a declarative multi-call runbook.
package cli

import (
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

// NewRootCommand builds the osapi command for the given version string. Errors
// are silenced here and printed by the caller so exit-code mapping stays in one
// place. `--version` prints the version; `osapi completion ...` is cobra's.
func NewRootCommand(version string) *cobra.Command {
	qf := &requestFlags{}
	root := &cobra.Command{
		Use:     "osapi",
		Short:   "A resilient CLI for OpenSearch REST endpoints",
		Version: version,
		Long: "Send a single request to an OpenSearch REST endpoint. The response body is " +
			"written to stdout (pipeable to jq); diagnostics go to stderr. Exit status is 0 " +
			"when the response is classified a success (2xx by default; see --success-when/" +
			"--retry-when) and 1 otherwise; the body is printed either way.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRequest(cmd, qf)
		},
	}

	registerConnFlags(root, &qf.connFlags)

	f := root.Flags()
	f.StringVarP(&qf.method, "method", "X", http.MethodGet, "HTTP method")
	f.StringVar(&qf.path, "path", "", "request path, e.g. _cluster/health")
	f.StringVarP(&qf.body, "body", "d", "",
		"request body: a literal string, @file to read a file, or @- to read stdin")
	f.BoolVar(&qf.bodySkeleton, "body-skeleton", false,
		"print a JSON request-body template for --path/-X and exit")
	f.StringArrayVarP(&qf.query, "query", "q", nil,
		"query parameter as key=value (repeatable)")
	f.StringArrayVarP(&qf.header, "header", "H", nil,
		"request header as \"Key: Value\" (repeatable)")
	f.IntVar(&qf.retry, config.FieldRetry, 0,
		"number of retries (0 = none; -1 = retry until success or an --abort-on status)")
	f.StringVar(&qf.backoff, config.FieldBackoff, config.Linear.String(),
		"backoff strategy between retries: constant, linear, or exponential")
	f.DurationVar(&qf.backoffInitial, config.FieldBackoffInitial, 2*time.Second,
		"initial backoff delay")
	f.DurationVar(&qf.backoffMax, config.FieldBackoffMax, 30*time.Second,
		"maximum backoff delay")
	f.Float64Var(&qf.backoffJitter, config.FieldBackoffJitter, 0,
		"backoff jitter as a fraction in [0,1)")
	f.IntSliceVar(&qf.abortOn, config.FieldAbortOn, nil,
		"status codes that stop retrying; comma-separated; only meaningful with --retry")
	f.StringVar(&qf.retryWhen, config.FieldRetryWhen, "",
		"jq expression evaluated against the JSON response body; truthy forces a retry even on 2xx")
	f.StringVar(&qf.successWhen, config.FieldSuccessWhen, "",
		"jq expression; the response counts as success only when truthy, regardless of status")
	f.StringVar(&qf.maxBodyBuffer, config.FieldMaxBodyBuffer, config.DefaultMaxBodyBuffer,
		"max response body buffered for --retry-when/--success-when evaluation (e.g. 10MiB; 0 = "+
			"unlimited); larger bodies skip predicate evaluation with a warning")

	registerCompletion(root, qf)
	_ = root.MarkFlagRequired("path")
	root.AddCommand(newRunCommand())
	return root
}
