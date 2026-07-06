// Package cli builds the osapi command: a resilient client for OpenSearch REST
// endpoints with configurable retry. osapi is a single-purpose tool, so the root
// command itself sends the request (there is no "request" subcommand).
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
			"for a 2xx response and 1 otherwise; the body is printed either way.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRequest(cmd, qf)
		},
	}

	f := root.Flags()
	f.StringVar(&qf.endpoint, config.FieldEndpoint, "",
		"OpenSearch endpoint URL (e.g. https://localhost:9200)")
	f.StringVarP(&qf.username, config.FieldUsername, "u", "",
		"username for basic authentication")
	f.StringVar(&qf.password, config.FieldPassword, "",
		"password for basic auth (visible in ps output and shell history; "+
			"prefer OPENSEARCH_PASSWORD, --env-file, or the interactive prompt)")
	f.StringVar(&qf.caCert, config.FieldCACert, "",
		"verify the server's TLS certificate against this CA bundle (PEM) instead "+
			"of the system roots; use it for a private/self-signed cluster CA")
	f.BoolVarP(&qf.insecure, config.FieldInsecure, "k", false,
		"skip TLS certificate verification")
	f.BoolVarP(&qf.verbose, "verbose", "v", false,
		"print per-attempt retry detail to stderr")
	f.StringVar(&qf.envFile, "env-file", "",
		"path to a dotenv file providing OPENSEARCH_URL/USERNAME/PASSWORD")
	f.StringVarP(&qf.method, "method", "X", http.MethodGet, "HTTP method")
	f.StringVar(&qf.path, "path", "", "request path, e.g. _cluster/health")
	f.StringVarP(&qf.body, "body", "d", "",
		"request body: a literal string, @file to read a file, or @- to read stdin")
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

	registerCompletion(root, qf)
	_ = root.MarkFlagRequired("path")
	return root
}
