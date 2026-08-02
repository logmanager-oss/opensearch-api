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
	"golang.org/x/term"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// requestFlags holds the request subcommand's flags: connection settings plus
// the request itself.
type requestFlags struct {
	endpoint string
	username string
	password string
	caCert   string
	insecure bool
	verbose  bool
	envFile  string

	method         string
	path           string
	body           string
	bodySkeleton   bool
	query          []string
	header         []string
	retry          int
	backoff        string
	backoffInitial time.Duration
	backoffMax     time.Duration
	backoffJitter  float64
	abortOn        []int
	retryWhen      string
	successWhen    string
	maxBodyBuffer  string
}

func runRequest(cmd *cobra.Command, qf *requestFlags) error {
	if qf.bodySkeleton {
		return printBodySkeleton(cmd.OutOrStdout(), cmd.ErrOrStderr(), qf.path, qf.method)
	}

	ctx := cmd.Context()

	cfg, retryWhen, successWhen, err := resolveConfig(cmd, qf)
	if err != nil {
		return err
	}

	client, err := osclient.New(osclient.Options{
		Endpoint:   cfg.Endpoint,
		Username:   cfg.Username,
		Password:   cfg.Password,
		CACertPath: cfg.CACertPath,
		Insecure:   cfg.Insecure,
		Warn:       cmd.ErrOrStderr(),
	})
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

	engine := retry.New(cfg.Retry,
		retry.WithOnRetry(verboseHook(cmd.ErrOrStderr(), qf.verbose)),
		retry.WithRetryWhen(retryWhen), retry.WithSuccessWhen(successWhen),
		retry.WithWarn(cmd.ErrOrStderr()))
	resp, doErr := engine.Do(ctx, func(ctx context.Context) (*http.Response, error) {
		c := req.Clone(ctx)
		if req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("preparing request body: %w", err)
			}
			c.Body = b
		}
		return client.Do(c)
	})

	// Stream the final body to stdout even on failure so 4xx/5xx payloads are
	// still visible; the engine error is returned unchanged for exit mapping.
	if resp != nil {
		_, copyErr := io.Copy(cmd.OutOrStdout(), resp.Body)
		_ = resp.Body.Close()
		if doErr == nil && copyErr != nil {
			return fmt.Errorf("writing response body: %w", copyErr)
		}
	}
	return doErr
}

// resolveConfig merges flags, env file, process env and defaults, compiles
// the body predicates, then resolves the password (prompting only on a TTY).
func resolveConfig(cmd *cobra.Command, qf *requestFlags) (_ config.Config, retryWhen, successWhen *retry.Predicate, _ error) {
	var fileVars map[string]string
	if qf.envFile != "" {
		vars, err := config.LoadEnvFile(qf.envFile)
		if err != nil {
			return config.Config{}, nil, nil, err
		}
		fileVars = vars
	}
	env := config.LayeredEnv(fileVars, os.LookupEnv)

	strategy, err := config.ParseBackoffStrategy(qf.backoff)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	maxBodyBuffer, err := config.ParseSize(qf.maxBodyBuffer)
	if err != nil {
		return config.Config{}, nil, nil, fmt.Errorf("invalid --max-body-buffer: %w", err)
	}

	flags := config.Config{
		Endpoint:   qf.endpoint,
		Username:   qf.username,
		Password:   qf.password,
		CACertPath: qf.caCert,
		Insecure:   qf.insecure,
		Retry: config.RetryConfig{
			MaxRetries:    qf.retry,
			Strategy:      strategy,
			Initial:       qf.backoffInitial,
			Max:           qf.backoffMax,
			Jitter:        qf.backoffJitter,
			AbortOn:       qf.abortOn,
			RetryWhen:     qf.retryWhen,
			SuccessWhen:   qf.successWhen,
			MaxBodyBuffer: maxBodyBuffer,
		},
	}

	cfg, err := config.Resolve(config.Sources{
		Flags:   flags,
		Changed: cmd.Flags().Changed,
		Env:     env,
	})
	if err != nil {
		return config.Config{}, nil, nil, err
	}

	// Compile the predicates before resolving the password, so a bad jq
	// expression is reported without first prompting interactively.
	retryWhen, err = retry.CompilePredicate(cfg.Retry.RetryWhen)
	if err != nil {
		return config.Config{}, nil, nil, fmt.Errorf("invalid --retry-when: %w", err)
	}
	successWhen, err = retry.CompilePredicate(cfg.Retry.SuccessWhen)
	if err != nil {
		return config.Config{}, nil, nil, fmt.Errorf("invalid --success-when: %w", err)
	}

	pw, err := config.ResolvePassword(cfg, config.TerminalPrompt(cfg.Username), isTerminal(cmd.InOrStdin()))
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	cfg.Password = pw
	return cfg, retryWhen, successWhen, nil
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
	return term.IsTerminal(int(f.Fd()))
}
