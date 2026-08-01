//go:build e2e

// Package e2e drives the built osapi binary as a subprocess against the live
// stack started by `make e2e-up` (see e2e/README.md). It never imports the
// module's internal packages: from here osapi is a black box invoked exactly
// like a user would invoke it.
package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Synthetic e2e-only fixture credentials; see e2e/securityconfig.
const (
	adminUser  = "admin"
	adminPass  = "E2e-Test-Only-Admin-Pw!"
	readerUser = "ci_reader"
	readerPass = "E2e-Test-Only-Reader-Pw!"
	lockedUser = "ci_locked"
	lockedPass = "E2e-Test-Only-Locked-Pw!"
)

// Populated once by TestMain; read-only for the rest of the package.
var (
	osapiBin        string
	baseURL         string
	caCertPath      string
	wrongCACertPath string
)

// result is the outcome of running the osapi binary once.
type result struct {
	stdout   string
	stderr   string
	exitCode int
}

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	baseURL = os.Getenv("E2E_OPENSEARCH_URL")
	if baseURL == "" {
		fmt.Fprintln(os.Stderr,
			"E2E_OPENSEARCH_URL is not set; run via `make e2e`, or `make e2e-up` then `make e2e-test`")
		return 1
	}

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolving working directory:", err)
		return 1
	}
	caCertPath = filepath.Join(wd, "certs", "ca.pem")
	wrongCACertPath = filepath.Join(wd, "certs", "wrong-ca.pem")

	osapiBin = filepath.Join(wd, "..", "osapi")
	if _, err := os.Stat(osapiBin); err != nil {
		fmt.Fprintln(os.Stderr,
			"osapi binary not found at", osapiBin, "- run `make build` first (`make e2e`/`make e2e-test` do)")
		return 1
	}

	if err := smokeTest(); err != nil {
		fmt.Fprintln(os.Stderr, "smoke test against", baseURL, "failed:", err)
		return 1
	}

	return m.Run()
}

// smokeTest confirms the stack and binary are usable before running the suite,
// so a misconfigured environment fails with one clear message instead of every
// test failing individually.
func smokeTest() error {
	res, err := execOsapi(nil, adminArgs(
		"--retry", "10", "--backoff", "constant", "--backoff-initial", "1s",
		"--path", "_cluster/health")...)
	if err != nil {
		return err
	}
	if res.exitCode != 0 {
		return fmt.Errorf("exit %d, stderr: %s", res.exitCode, res.stderr)
	}
	return nil
}

// runOsapi runs the osapi binary and fails the test on anything but a normal
// exit (including a non-zero exit code, which callers assert on explicitly).
// A nil env means the scrubbed base env with no additions; a non-nil map is
// layered on top of that same base.
func runOsapi(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	res, err := execOsapi(env, args...)
	require.NoError(t, err)
	return res
}

// execOsapi is the env-and-exec-error-agnostic core of runOsapi, usable from
// TestMain (which has no *testing.T) as well.
func execOsapi(env map[string]string, args ...string) (result, error) {
	cmd := exec.Command(osapiBin, args...)
	cmd.Env = scrubbedEnv(env)
	cmd.Stdin = nil // reads from the null device: guaranteed non-TTY.

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	res := result{}
	err := cmd.Run()
	res.stdout, res.stderr = stdout.String(), stderr.String()

	var exitErr *exec.ExitError
	if err == nil {
		return res, nil
	}
	if errors.As(err, &exitErr) {
		res.exitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("running osapi: %w", err)
}

// scrubbedEnv builds a child environment carrying over only PATH/HOME/TMPDIR
// from the test process, then layers overrides on top. Critically, this drops
// every OPENSEARCH_* variable the parent process might have set, so config
// tests observe only what they explicitly configure.
func scrubbedEnv(overrides map[string]string) []string {
	carried := []string{"PATH", "HOME", "TMPDIR"}
	base := make(map[string]string, len(carried)+len(overrides))
	for _, k := range carried {
		if v, ok := os.LookupEnv(k); ok {
			base[k] = v
		}
	}
	maps.Copy(base, overrides)

	env := make([]string, 0, len(base))
	for k, v := range base {
		env = append(env, k+"="+v)
	}
	return env
}

// adminArgs prepends connection flags for the admin user to extra.
func adminArgs(extra ...string) []string {
	return asUser(adminUser, adminPass, extra...)
}

// asUser prepends connection flags for user/pass to extra: --endpoint,
// credentials, and --ca-cert so callers don't repeat the trust anchor.
func asUser(user, pass string, extra ...string) []string {
	args := make([]string, 0, 8+len(extra))
	args = append(args,
		"--endpoint", baseURL,
		"-u", user,
		"--password", pass,
		"--ca-cert", caCertPath,
	)
	return append(args, extra...)
}
