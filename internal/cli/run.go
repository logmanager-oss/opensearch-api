package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/runbook"
)

// runFlags holds the run subcommand's flags: connection settings (embedded,
// shared with root) plus --dry-run. Retry is YAML-only for run: it comes from
// each call's own retry keys, not from flags.
type runFlags struct {
	connFlags
	dryRun bool
}

// newRunCommand builds the "run" subcommand, which executes a YAML-defined
// runbook of calls against a single OpenSearch endpoint.
func newRunCommand() *cobra.Command {
	rf := &runFlags{}
	cmd := &cobra.Command{
		Use:   "run <file.yaml>",
		Short: "Run a YAML-defined runbook of calls against an OpenSearch endpoint",
		Long: "Execute an ordered sequence of OpenSearch calls defined in a YAML runbook. " +
			"Per-call retry is configured in the YAML file itself, not via flags. " +
			"--dry-run prints the execution plan without connecting to the endpoint.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunbook(cmd, rf, args[0])
		},
	}

	registerConnFlags(cmd, &rf.connFlags)
	cmd.Flags().BoolVar(&rf.dryRun, "dry-run", false, "print the execution plan and exit; no connection is made")

	return cmd
}

// runRunbook loads the runbook at path, honours --dry-run, then resolves the
// connection and executes it. The file is loaded before any connection
// resolution or password prompt, so a typo'd runbook fails fast.
func runRunbook(cmd *cobra.Command, rf *runFlags, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening runbook %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Relative @file bodies resolve against the runbook's own directory, so a
	// runbook and its body files stay portable together.
	rb, err := runbook.Load(f, filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("loading runbook %s: %w", path, err)
	}

	if rf.dryRun {
		printPlan(cmd.ErrOrStderr(), rb)
		return nil
	}

	cfg, err := resolveConnection(cmd, &rf.connFlags)
	if err != nil {
		return err
	}

	client, err := newClient(cmd, &cfg)
	if err != nil {
		return err
	}

	runner := &runbook.Runner{
		Client:   client,
		Endpoint: cfg.Endpoint,
		Progress: cmd.ErrOrStderr(),
		Verbose:  rf.verbose,
		Warn:     cmd.ErrOrStderr(),
	}
	return runner.Run(cmd.Context(), rb)
}

// resolveConnection merges flags, env file, process env and defaults into a
// Config, then resolves the password (prompting only on a TTY). It mirrors
// resolveConfig's connection-resolution steps, minus the retry/predicate
// parts that don't apply to run: retry is YAML-only there.
func resolveConnection(cmd *cobra.Command, c *connFlags) (config.Config, error) {
	var fileVars map[string]string
	if c.envFile != "" {
		vars, err := config.LoadEnvFile(c.envFile)
		if err != nil {
			return config.Config{}, err
		}
		fileVars = vars
	}
	env := config.LayeredEnv(fileVars, os.LookupEnv)

	flags := config.Config{
		Endpoint:   c.endpoint,
		Username:   c.username,
		Password:   c.password,
		CACertPath: c.caCert,
		Insecure:   c.insecure,
	}

	cfg, err := config.Resolve(config.Sources{
		Flags:   flags,
		Changed: cmd.Flags().Changed,
		Env:     env,
	})
	if err != nil {
		return config.Config{}, err
	}

	pw, err := config.ResolvePassword(cfg, config.TerminalPrompt(cfg.Username), isTerminal(cmd.InOrStdin()))
	if err != nil {
		return config.Config{}, err
	}
	cfg.Password = pw
	return cfg, nil
}

// printPlan writes rb's execution plan to w: each entry call in document
// order, with its depends-on and verify-with wiring.
func printPlan(w io.Writer, rb *runbook.Runbook) {
	_, _ = fmt.Fprintln(w, "plan:")
	for n, idx := range rb.Entries {
		call := &rb.Calls[idx]
		_, _ = fmt.Fprintf(w, "%d. call %q: %s %s\n", n+1, call.Name, call.Method, call.Path)
		if len(call.DependsOn) > 0 {
			_, _ = fmt.Fprintf(w, "   depends-on: %s\n", strings.Join(call.DependsOn, ", "))
		}
		if call.VerifyWith != "" {
			_, _ = fmt.Fprintf(w, "   verify-with: %s\n", call.VerifyWith)
		}
	}
}
