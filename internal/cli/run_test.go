package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

func writeRunbook(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "runbook.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestRunSuccess(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK, http.StatusOK})

	path := writeRunbook(t, t.TempDir(), `
calls:
  - name: health
    method: GET
    path: /_cluster/health
  - name: create_index
    method: PUT
    path: /my-index
`)

	stdout, stderr, err := run(t, nil, "run", path, "--endpoint", srv.URL)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, `call "health": ok`)
	assert.Contains(t, stderr, `call "create_index": ok`)
	assert.Contains(t, stderr, "run: 2 succeeded")
	assert.Equal(t, 2, rec.len())
}

func TestRunFailingCall(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusInternalServerError})

	path := writeRunbook(t, t.TempDir(), `
calls:
  - name: check
    method: GET
    path: /x
`)

	_, stderr, err := run(t, nil, "run", path, "--endpoint", srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"check"`)
	assert.Contains(t, stderr, "body-500")
	assert.Equal(t, 1, rec.len())
	// The Runner wrote the failure to stderr; main must not print it again.
	assert.True(t, IsReported(err))
}

func TestRunMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	_, _, err := run(t, nil, "run", missing, "--endpoint", "http://localhost:9200")
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
	// A load error reaches stderr only through main's print.
	assert.False(t, IsReported(err))
}

// IsReported must not hide the cause: main maps context.Canceled to exit
// code 130 via errors.Is.
func TestReportedErrorPreservesCause(t *testing.T) {
	err := &reportedError{err: context.Canceled}
	assert.True(t, IsReported(err))
	assert.ErrorIs(t, err, context.Canceled)
}

// A bad runbook must fail before password resolution: with -u and no
// --password on a non-TTY stdin, resolveConnection running first would fail
// with config.ErrNoPassword instead of the load error.
func TestRunLoadErrorBeatsPasswordResolution(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	_, _, err := run(t, nil, "run", missing, "--endpoint", "http://localhost:9200", "-u", "admin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
	assert.NotContains(t, err.Error(), "password")
}

// TestRunBodyFileRelativeToRunbookDir proves LoadFile's baseDir wiring: with
// cwd set elsewhere, a body naming a sibling file by bare filename still
// resolves against the runbook's directory.
func TestRunBodyFileRelativeToRunbookDir(t *testing.T) {
	runbookDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(runbookDir, "payload.json"), []byte(`{"a":1}`), 0o600))
	path := writeRunbook(t, runbookDir, `
calls:
  - name: create
    method: POST
    path: /my-index/_doc
    body: '@payload.json'
`)

	t.Chdir(t.TempDir())

	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})

	_, _, err := run(t, nil, "run", path, "--endpoint", srv.URL)
	require.NoError(t, err)
	require.Equal(t, 1, rec.len())
	assert.Equal(t, `{"a":1}`, string(rec.at(0).body))
}

func TestRunLoadErrorsSendNoRequests(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "typo key",
			content: `
calls:
  - name: x
    methdo: GET
    path: /x
`,
		},
		{
			name: "unknown capture reference",
			content: `
calls:
  - name: x
    method: GET
    path: /x/${missing}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec capture
			srv := newServer(t, &rec, []int{http.StatusOK})
			path := writeRunbook(t, t.TempDir(), tt.content)

			_, _, err := run(t, nil, "run", path, "--endpoint", srv.URL)
			require.Error(t, err)
			assert.Equal(t, 0, rec.len())
		})
	}
}

func TestRunDryRun(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})

	path := writeRunbook(t, t.TempDir(), `
calls:
  - name: get_id
    method: GET
    path: /thing
    capture:
      thing_id: '.id'
  - name: use_id
    method: GET
    path: /thing/${thing_id}
    continue-on-failure: true
`)

	// -u admin with no password on a non-TTY stdin would fail password
	// resolution if it ran. Dry-run must never reach it.
	stdout, stderr, err := run(t, nil, "run", path, "--dry-run",
		"--endpoint", srv.URL, "-u", "admin")
	require.NoError(t, err)
	assert.Empty(t, stdout)
	// Asserted as whole lines: a bare stderr Contains("thing_id") would already
	// pass via call 2's path, even with produces/consumes deleted from printPlan.
	assert.Equal(t, strings.Join([]string{
		"dry-run: 2 call(s), no requests sent",
		"  1. get_id: GET /thing",
		"     produces: thing_id",
		"  2. use_id: GET /thing/${thing_id} (continue-on-failure)",
		"     consumes: ${thing_id}",
		"",
	}, "\n"), stderr)
	assert.Equal(t, 0, rec.len())
}

// Checks that body/retry inherited from defaults: appear in the plan. Headers
// must never appear. Printing them would leak Authorization to CI logs.
func TestRunDryRunShowsBodyAndRetryFromDefaults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "payload.json"), []byte(`{"doc":1}`), 0o600))
	path := writeRunbook(t, dir, `
defaults:
  method: DELETE
  retry: 3
  backoff: exponential
  body: '@payload.json'
  headers:
    Authorization: Basic c3VwZXItc2VjcmV0
calls:
  - name: wipe
    path: /index-a
  - name: forever
    path: /index-b
    retry: -1
`)

	_, stderr, err := run(t, nil, "run", path, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, stderr, "  1. wipe: DELETE /index-a")
	assert.Contains(t, stderr, "     body: 9 bytes")
	assert.Contains(t, stderr, "     retry: 3 (exponential)")
	assert.Contains(t, stderr, "     retry: unlimited (exponential)")
	assert.NotContains(t, stderr, "c3VwZXItc2VjcmV0", "headers must never reach the plan")
	assert.NotContains(t, stderr, "Authorization")
}

// Underpins the ordering tests: -u with no password on a non-TTY fails
// password resolution. Without this test, removing ResolvePassword would
// leave those tests green without asserting anything.
func TestRunPasswordResolutionFailsWithoutPassword(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})
	path := writeRunbook(t, t.TempDir(), "calls:\n  - name: c\n    path: /x\n")

	_, _, err := run(t, nil, "run", path, "--endpoint", srv.URL, "-u", "admin")
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrNoPassword)
	assert.Equal(t, 0, rec.len())
}

// The endpoint may come from the environment, not just --endpoint: run
// layers env exactly like the root command.
func TestRunEndpointFromEnvFile(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})

	dir := t.TempDir()
	envPath := filepath.Join(dir, "osapi.env")
	require.NoError(t, os.WriteFile(envPath,
		[]byte("OPENSEARCH_URL="+srv.URL+"\nOPENSEARCH_USERNAME=admin\nOPENSEARCH_PASSWORD=secret\n"), 0o600))
	path := writeRunbook(t, dir, "calls:\n  - name: c\n    path: /x\n")

	stdout, stderr, err := run(t, nil, "run", path, "--env-file", envPath)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, `call "c": ok`)
	require.Equal(t, 1, rec.len())
	assert.Equal(t, "admin", rec.at(0).user)
	assert.Equal(t, "secret", rec.at(0).pass)
}

// RunE indexes args[0]. Without ExactArgs(1), a bare `osapi run` panics.
func TestRunRequiresExactlyOneArg(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", "a.yaml", "b.yaml"}} {
		_, _, err := run(t, nil, args...)
		require.Error(t, err)
		assert.ErrorContains(t, err, "accepts 1 arg")
	}
}

func TestRunDryRunErrors(t *testing.T) {
	t.Run("invalid file", func(t *testing.T) {
		path := writeRunbook(t, t.TempDir(), `
calls:
  - name: x
    methdo: GET
    path: /x
`)
		_, _, err := run(t, nil, "run", path, "--dry-run")
		require.Error(t, err)
	})

	t.Run("missing env-file", func(t *testing.T) {
		path := writeRunbook(t, t.TempDir(), `
calls:
  - name: x
    method: GET
    path: /x
`)
		_, _, err := run(t, nil, "run", path, "--dry-run",
			"--env-file", filepath.Join(t.TempDir(), "nope.env"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "env file")
	})
}

func TestRunFlagPosition(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK, http.StatusOK})

	path := writeRunbook(t, t.TempDir(), `
calls:
  - name: health
    method: GET
    path: /_cluster/health
`)

	_, _, err := run(t, nil, "run", path,
		"--endpoint", srv.URL, "-u", "admin", "--password", "secret")
	require.NoError(t, err)

	_, _, err = run(t, nil,
		"--endpoint", srv.URL, "-u", "admin", "--password", "secret", "run", path)
	require.NoError(t, err)

	require.Equal(t, 2, rec.len())
	assert.True(t, rec.at(0).authOK)
	assert.True(t, rec.at(1).authOK)
}

func TestRunRejectsRootOnlyFlags(t *testing.T) {
	path := writeRunbook(t, t.TempDir(), `
calls:
  - name: x
    method: GET
    path: /x
`)

	tests := [][]string{
		{"run", path, "--retry", "3"},
		{"--retry", "3", "run", path},
		{"run", path, "--path", "/x"},
	}
	for _, args := range tests {
		t.Run(args[0]+"_"+args[len(args)-1], func(t *testing.T) {
			_, stderr, err := run(t, nil, args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unknown flag")
			assert.NotContains(t, stderr, "Usage:", "SilenceUsage must match root")
		})
	}
}

func TestRunVerboseRetryAndCapture(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec,
		[]int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusOK},
		[]string{"", "", `{"status":"green"}`})

	path := writeRunbook(t, t.TempDir(), `
calls:
  - name: probe
    method: GET
    path: /probe
    retry: 2
    backoff-initial: '1ms'
    capture:
      status_val: '.status'
`)

	_, stderr, err := run(t, nil, "run", path, "--endpoint", srv.URL, "-v")
	require.NoError(t, err)
	assert.Contains(t, stderr, "attempt 1")
	assert.Contains(t, stderr, "status_val=green")
	assert.Equal(t, 3, rec.len())
}
