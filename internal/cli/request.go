package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// connFlags holds the flags every command needs regardless of what it sends:
// how to reach the cluster, plus verbose and env-file, which are not strictly
// connection settings but travel with the same group for the same reason.
type connFlags struct {
	endpoint string
	username string
	password string
	caCert   string
	insecure bool
	verbose  bool
	envFile  string
	timeout  time.Duration
}

// registerConnFlags declares the connection flags on f. Shorthands are part
// of the contract, not cosmetic: in `osapi --endpoint ... run f.yaml` cobra
// only uses root's flags to skip over flag arguments while locating the
// subcommand, then hands the remaining args — flags included — to that
// subcommand's flagset. So every name and shorthand must match root's exactly,
// or the leading flags fail to parse.
func registerConnFlags(f *pflag.FlagSet, c *connFlags) {
	f.StringVar(&c.endpoint, config.FieldEndpoint, config.DefaultEndpoint,
		"OpenSearch endpoint URL")
	f.StringVarP(&c.username, config.FieldUsername, "u", "",
		"username for basic authentication")
	f.StringVar(&c.password, config.FieldPassword, "",
		"password for basic auth (visible in ps output and shell history; "+
			"prefer OPENSEARCH_PASSWORD, --env-file, or the interactive prompt)")
	f.StringVar(&c.caCert, config.FieldCACert, "",
		"verify the server's TLS certificate against this CA bundle (PEM) instead "+
			"of the system roots; use it for a private/self-signed cluster CA")
	f.BoolVarP(&c.insecure, config.FieldInsecure, "k", false,
		"skip TLS certificate verification")
	f.BoolVarP(&c.verbose, "verbose", "v", false,
		"print per-attempt retry detail to stderr")
	f.StringVar(&c.envFile, "env-file", "",
		"path to a dotenv file providing OPENSEARCH_URL/USERNAME/PASSWORD and OSAPI_TIMEOUT")
	f.DurationVar(&c.timeout, config.FieldTimeout, 0,
		"bound the whole invocation, retries included, e.g. 30s (0 = no bound)")
}

// config returns the flag values as a Config for config.Resolve.
func (c *connFlags) config() config.Config {
	return config.Config{
		Endpoint:   c.endpoint,
		Username:   c.username,
		Password:   c.password,
		CACertPath: c.caCert,
		Insecure:   c.insecure,
		Timeout:    c.timeout,
	}
}

// newClient builds the OpenSearch client for cfg, warning on stderr.
func newClient(cmd *cobra.Command, cfg *config.Config) (*http.Client, error) {
	return osclient.New(osclient.Options{
		Endpoint:   cfg.Endpoint,
		Username:   cfg.Username,
		Password:   cfg.Password,
		CACertPath: cfg.CACertPath,
		Insecure:   cfg.Insecure,
		Warn:       cmd.ErrOrStderr(),
	})
}

// retryFlags holds the retry and response-classification flags.
type retryFlags struct {
	retry          int
	backoff        string
	backoffInitial time.Duration
	backoffMax     time.Duration
	backoffJitter  float64
	abortOn        []int
	retryOn        []int
	retryWhen      string
	successWhen    string
	maxBodyBuffer  string
}

// registerRetryFlags declares the retry flags on f and their mutual exclusion on cmd.
func registerRetryFlags(cmd *cobra.Command, f *pflag.FlagSet, r *retryFlags) {
	f.IntVar(&r.retry, config.FieldRetry, 0,
		"number of retries (0 = none; -1 = retry until success or a terminal status from --abort-on or --retry-on)")
	f.StringVar(&r.backoff, config.FieldBackoff, config.Linear.String(),
		"backoff strategy between retries: constant, linear, or exponential")
	f.DurationVar(&r.backoffInitial, config.FieldBackoffInitial, 2*time.Second,
		"initial backoff delay")
	f.DurationVar(&r.backoffMax, config.FieldBackoffMax, 30*time.Second,
		"maximum backoff delay")
	f.Float64Var(&r.backoffJitter, config.FieldBackoffJitter, 0,
		"backoff jitter as a fraction in [0,1)")
	f.IntSliceVar(&r.abortOn, config.FieldAbortOn, nil,
		"status codes that stop retrying; comma-separated; only meaningful with --retry")
	f.IntSliceVar(&r.retryOn, config.FieldRetryOn, nil,
		"non-2xx status codes that keep retrying; every other non-2xx stops "+
			"(comma-separated; only meaningful with --retry; excludes --abort-on)")
	f.StringVar(&r.retryWhen, config.FieldRetryWhen, "",
		"jq expression evaluated against the JSON response body; truthy forces a retry even on 2xx")
	f.StringVar(&r.successWhen, config.FieldSuccessWhen, "",
		"jq expression; the response counts as success only when truthy, regardless of status")
	f.StringVar(&r.maxBodyBuffer, config.FieldMaxBodyBuffer, config.DefaultMaxBodyBuffer,
		"max response body buffered for --retry-when/--success-when evaluation (e.g. 10MiB; 0 = "+
			"unlimited); larger bodies skip predicate evaluation with a warning")
	cmd.MarkFlagsMutuallyExclusive(config.FieldAbortOn, config.FieldRetryOn)
}

// requestFlags holds the root command's flags: the connection settings plus
// the request itself.
type requestFlags struct {
	connFlags
	retryFlags

	method       string
	path         string
	body         string
	bodySkeleton bool
	query        []string
	header       []string
}

// predicates bundles the compiled --retry-when/--success-when.
type predicates struct{ retryWhen, successWhen *retry.Predicate }

func runRequest(cmd *cobra.Command, qf *requestFlags) error {
	if qf.bodySkeleton {
		return printBodySkeleton(cmd.OutOrStdout(), cmd.ErrOrStderr(), qf.path, qf.method)
	}

	cfg, preds, err := resolveConfig(cmd, &qf.connFlags, &qf.retryFlags)
	if err != nil {
		return err
	}

	client, err := newClient(cmd, &cfg)
	if err != nil {
		return err
	}

	body, hasBody, err := osclient.ReadBody(qf.body, cmd.InOrStdin())
	if err != nil {
		return err
	}
	query, err := parseQuery(qf.query)
	if err != nil {
		return err
	}
	headers, err := parseHeaders(qf.header)
	if err != nil {
		return err
	}

	req, err := osclient.BuildRequest(cfg.Endpoint, osclient.RequestSpec{
		Method:  qf.method,
		Path:    qf.path,
		Body:    body,
		HasBody: hasBody,
		Query:   query,
		Headers: headers,
	})
	if err != nil {
		return err
	}

	ctx, cancel := withTimeout(cmd.Context(), cfg.Timeout)
	defer cancel()

	resp, doErr := doWithRetry(cmd, ctx, &cfg, preds, qf.verbose, osclient.Replay(client, req)) //nolint:bodyclose // streamBody closes it
	return streamBody(cmd, ctx, &cfg, resp, doErr)
}

// doWithRetry runs attempt under cfg.Retry with p and the verbose hook.
func doWithRetry(cmd *cobra.Command, ctx context.Context, cfg *config.Config, p predicates, verbose bool, attempt retry.Attempt) (*http.Response, error) {
	engine := retry.New(cfg.Retry,
		retry.WithOnRetry(verboseHook(cmd.ErrOrStderr(), verbose)),
		retry.WithRetryWhen(p.retryWhen), retry.WithSuccessWhen(p.successWhen),
		retry.WithWarn(cmd.ErrOrStderr()))
	return engine.Do(ctx, attempt)
}

// streamBody copies resp's body to stdout and maps doErr through timeoutError.
// It streams the body even on failure, so 4xx/5xx payloads stay visible.
// The engine error returns unchanged for exit mapping.
func streamBody(cmd *cobra.Command, ctx context.Context, cfg *config.Config, resp *http.Response, doErr error) error {
	if resp != nil {
		_, copyErr := io.Copy(cmd.OutOrStdout(), resp.Body)
		_ = resp.Body.Close()
		if doErr == nil && copyErr != nil {
			doErr = fmt.Errorf("writing response body: %w", copyErr)
		}
	}
	return timeoutError(ctx, cfg.Timeout, doErr)
}

// resolveConfig merges flags, env file, process env and defaults, compiles
// the body predicates, then resolves the password (prompting only on a TTY).
func resolveConfig(cmd *cobra.Command, cf *connFlags, rf *retryFlags) (config.Config, predicates, error) {
	env, err := loadEnv(cf.envFile)
	if err != nil {
		return config.Config{}, predicates{}, err
	}

	strategy, err := config.ParseBackoffStrategy(rf.backoff)
	if err != nil {
		return config.Config{}, predicates{}, err
	}
	maxBodyBuffer, err := config.ParseSize(rf.maxBodyBuffer)
	if err != nil {
		return config.Config{}, predicates{}, fmt.Errorf("invalid --max-body-buffer: %w", err)
	}

	flags := cf.config()
	flags.Retry = config.RetryConfig{
		MaxRetries:    rf.retry,
		Strategy:      strategy,
		Initial:       rf.backoffInitial,
		Max:           rf.backoffMax,
		Jitter:        rf.backoffJitter,
		AbortOn:       rf.abortOn,
		RetryOn:       rf.retryOn,
		RetryWhen:     rf.retryWhen,
		SuccessWhen:   rf.successWhen,
		MaxBodyBuffer: maxBodyBuffer,
	}

	cfg, err := config.Resolve(config.Sources{
		Flags:   flags,
		Changed: cmd.Flags().Changed,
		Env:     env,
	})
	if err != nil {
		return config.Config{}, predicates{}, err
	}

	var p predicates
	// Compile the predicates before resolving the password, so a bad jq
	// expression is reported without first prompting interactively.
	p.retryWhen, err = retry.CompilePredicate(cfg.Retry.RetryWhen)
	if err != nil {
		return config.Config{}, predicates{}, fmt.Errorf("invalid --retry-when: %w", err)
	}
	p.successWhen, err = retry.CompilePredicate(cfg.Retry.SuccessWhen)
	if err != nil {
		return config.Config{}, predicates{}, fmt.Errorf("invalid --success-when: %w", err)
	}

	pw, err := config.ResolvePassword(cfg, config.TerminalPrompt(cfg.Username), isTerminal(cmd.InOrStdin()))
	if err != nil {
		return config.Config{}, predicates{}, err
	}
	cfg.Password = pw
	return cfg, p, nil
}

// parseQuery parses repeated key=value pairs into a query map.
func parseQuery(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --query %q: want key=value", p)
		}
		out[k] = v
	}
	return out, nil
}

// parseHeaders parses repeated "Key: Value" pairs, preserving colons in the
// value and one optional leading space after the separator.
func parseHeaders(pairs []string) (http.Header, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	h := make(http.Header, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, ":")
		if !ok {
			return nil, fmt.Errorf("invalid --header %q: want \"Key: Value\"", p)
		}
		h.Add(strings.TrimSpace(k), strings.TrimPrefix(v, " "))
	}
	return h, nil
}

// isTerminal reports whether r is an interactive terminal.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd())) //nolint:gosec // G115: a file fd fits int on every supported platform
}
