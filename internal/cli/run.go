package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/runbook"
)

// runFlags holds the run subcommand's flags: the connection settings plus
// --dry-run.
type runFlags struct {
	connFlags

	dryRun bool
}

// newRunCommand builds the `osapi run <file.yaml>` subcommand: it runs a
// declarative runbook (see internal/runbook) with the same connection
// settings as root.
func newRunCommand() *cobra.Command {
	rf := &runFlags{}
	cmd := &cobra.Command{
		Use:   "run <file.yaml>",
		Short: "Run a declarative multi-call runbook",
		Long: "Run a sequence of OpenSearch calls declared in a YAML runbook, in document " +
			"order. Each call's outcome and the run summary are written to stderr; " +
			"--dry-run validates the file and prints the plan without sending anything.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp // ExactArgs(1) would reject a second one
			}
			return []string{"yaml", "yml"}, cobra.ShellCompDirectiveFilterFileExt
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRun(cmd, rf, args[0])
		},
	}

	registerConnFlags(cmd, &rf.connFlags)
	cmd.Flags().BoolVar(&rf.dryRun, "dry-run", false,
		"validate the runbook and print its plan without sending any request")

	return cmd
}

// runRun implements `osapi run`. It loads the runbook and env file before
// checking --dry-run, and --dry-run returns before the password is
// resolved.
func runRun(cmd *cobra.Command, rf *runFlags, path string) error {
	rb, err := runbook.LoadFile(path)
	if err != nil {
		return err
	}

	env, err := loadEnv(rf.envFile)
	if err != nil {
		return err
	}

	if rf.dryRun {
		printPlan(cmd.ErrOrStderr(), rb)
		return nil
	}

	var creds runbook.Credentials
	if rb.Credentials != nil {
		creds, err = rb.Credentials.Resolve(env)
		if err != nil {
			return fmt.Errorf("resolving runbook credentials: %w", err)
		}
	}

	cfg, err := resolveConnection(cmd, &rf.connFlags, env, rb.Credentials == nil)
	if err != nil {
		return err
	}

	// Folded into cfg rather than carried alongside it: anything reading
	// cfg.Username later must see the identity that actually authenticates.
	if rb.Credentials != nil {
		cfg.Username, cfg.Password = creds.Username, creds.Password
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

	runner := &runbook.Runner{
		Client:   client,
		Endpoint: cfg.Endpoint,
		Stderr:   cmd.ErrOrStderr(),
		Verbose:  rf.verbose,
	}
	if err := runner.Run(cmd.Context(), rb); err != nil {
		return &reportedError{err: err}
	}
	return nil
}

// reportedError wraps a Runner.Run error: the Runner already wrote the
// failure and summary to Stderr, so main must not print it again.
type reportedError struct{ err error }

func (e *reportedError) Error() string { return e.err.Error() }

// Unwrap keeps the cause visible: main maps context.Canceled to exit code
// 130 via errors.Is.
func (e *reportedError) Unwrap() error { return e.err }

// IsReported reports whether err was already written to stderr, so main
// must not print it again.
func IsReported(err error) bool {
	var re *reportedError
	return errors.As(err, &re)
}

// loadEnv layers an optional --env-file over the process environment,
// mirroring resolveConfig's env step (request.go).
func loadEnv(envFile string) (config.EnvLookup, error) {
	var fileVars map[string]string
	if envFile != "" {
		vars, err := config.LoadEnvFile(envFile)
		if err != nil {
			return nil, err
		}
		fileVars = vars
	}
	return config.LayeredEnv(fileVars, os.LookupEnv), nil
}

// resolveConnection resolves cf against env and flag precedence, then
// resolves the password, prompting only on a TTY. It mirrors resolveConfig
// (request.go) minus the retry/predicate parts, which run lacks flags for.
// needPassword is false when the runbook supplies credentials: no password
// prompt, no requirement.
func resolveConnection(cmd *cobra.Command, cf *connFlags, env config.EnvLookup, needPassword bool) (config.Config, error) {
	flags := config.Config{
		Endpoint:   cf.endpoint,
		Username:   cf.username,
		Password:   cf.password,
		CACertPath: cf.caCert,
		Insecure:   cf.insecure,
	}
	cfg, err := config.Resolve(config.Sources{
		Flags:   flags,
		Changed: cmd.Flags().Changed,
		Env:     env,
	})
	if err != nil {
		return config.Config{}, err
	}
	if !needPassword {
		return cfg, nil
	}

	pw, err := config.ResolvePassword(cfg, config.TerminalPrompt(cfg.Username), isTerminal(cmd.InOrStdin()))
	if err != nil {
		return config.Config{}, err
	}
	cfg.Password = pw
	return cfg, nil
}

// printPlan writes rb's dry-run plan to w: each call's method, path,
// produced captures, consumed ${...} names, and a continue-on-failure
// marker. It uses Call.References rather than a separate scanner, so the two
// can't disagree.
func printPlan(w io.Writer, rb *runbook.Runbook) {
	_, _ = fmt.Fprintf(w, "dry-run: %d call(s), no requests sent\n", len(rb.Calls))
	if rb.Credentials != nil {
		_, _ = fmt.Fprintln(w, "  credentials: defined by runbook (overrides -u/--password and OPENSEARCH_USERNAME/PASSWORD)")
	}
	for i := range rb.Calls {
		c := &rb.Calls[i]
		line := fmt.Sprintf("  %d. %s: %s %s", i+1, c.Name, c.Method, c.Path)
		if c.ContinueOnFailure {
			line += " (continue-on-failure)"
		}
		_, _ = fmt.Fprintln(w, line)

		// Prints body size only, not content, and never headers: a dry-run only
		// needs to show whether a call writes something and how hard it
		// retries. Headers would leak Authorization to stderr and CI logs.
		if c.HasBody {
			_, _ = fmt.Fprintf(w, "     body: %d bytes\n", len(c.Body))
		}
		if c.Retry.MaxRetries != 0 {
			_, _ = fmt.Fprintf(w, "     retry: %s (%s)\n", retryCount(c.Retry.MaxRetries), c.Retry.Strategy)
		}
		if len(c.Capture) > 0 {
			names := make([]string, len(c.Capture))
			for j, cp := range c.Capture {
				names[j] = cp.Name
			}
			_, _ = fmt.Fprintf(w, "     produces: %s\n", strings.Join(names, ", "))
		}
		if refs := c.References(); len(refs) > 0 {
			_, _ = fmt.Fprintf(w, "     consumes: %s\n", formatRefs(refs))
		}
	}
}

// retryCount renders a retry budget for the plan. Negative means unlimited,
// printed as "unlimited" instead of -1.
func retryCount(n int) string {
	if n < 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

// formatRefs renders capture names as ${name} for the dry-run plan.
func formatRefs(names []string) string {
	wrapped := make([]string, len(names))
	for i, n := range names {
		wrapped[i] = "${" + n + "}"
	}
	return strings.Join(wrapped, ", ")
}
