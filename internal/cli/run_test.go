package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeRunbook writes content to a temp .yaml file and returns its path.
func writeRunbook(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runbook.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestRunSuccess(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})
	path := writeRunbook(t, `
calls:
  health:
    path: _cluster/health
`)

	stdout, stderr, err := run(t, nil, "run", path, "--endpoint", srv.URL)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, `call "health"`)
	assert.Contains(t, stderr, "run: 1 succeeded, 0 failed, 0 skipped")
	assert.Equal(t, 1, rec.len())
}

func TestRunFailingCall(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusServiceUnavailable})
	path := writeRunbook(t, `
calls:
  health:
    path: _cluster/health
`)

	_, stderr, err := run(t, nil, "run", path, "--endpoint", srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"health"`)
	assert.Contains(t, stderr, `call "health": failed`)
}

func TestRunMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	_, _, err := run(t, nil, "run", missing, "--endpoint", "http://localhost:9200")
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
}

func TestRunTypoKeyFailsBeforeHTTP(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})
	path := writeRunbook(t, `
calls:
  health:
    pathh: _cluster/health
`)

	_, _, err := run(t, nil, "run", path, "--endpoint", srv.URL)
	require.Error(t, err)
	assert.Equal(t, 0, rec.len(), "load error must happen before any HTTP request")
}

func TestRunDryRunValidFile(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})
	path := writeRunbook(t, `
calls:
  health:
    path: _cluster/health
`)

	// -u with no password would fail off-TTY if connection resolution ran;
	// dry-run must skip it entirely, so this succeeds.
	stdout, stderr, err := run(t, nil, "run", path, "--dry-run", "--endpoint", srv.URL, "-u", "admin")
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, `call "health"`)
	assert.Contains(t, stderr, "_cluster/health")
	assert.Equal(t, 0, rec.len(), "dry-run must send no HTTP requests")
}

func TestRunDryRunInvalidFile(t *testing.T) {
	path := writeRunbook(t, `
calls:
  health:
    pathh: _cluster/health
`)

	_, _, err := run(t, nil, "run", path, "--dry-run")
	require.Error(t, err)
}

func TestRunDryRunShowsDependsOnAndVerifyWith(t *testing.T) {
	path := writeRunbook(t, `
calls:
  create:
    path: my-index
    method: PUT
    verify-with: check
  check:
    path: my-index
  use:
    path: my-index/_doc
    method: POST
    depends-on: create
`)

	_, stderr, err := run(t, nil, "run", path, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, stderr, `call "create"`)
	assert.Contains(t, stderr, "verify-with: check")
	assert.Contains(t, stderr, `call "use"`)
	assert.Contains(t, stderr, "depends-on: create")
}

func TestRunConnectionFlagsReachServer(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})
	path := writeRunbook(t, `
calls:
  health:
    path: _cluster/health
`)

	_, _, err := run(t, nil, "run", path,
		"--endpoint", srv.URL, "-u", "admin", "--password", "secret")
	require.NoError(t, err)
	require.Equal(t, 1, rec.len())
	got := rec.at(0)
	assert.True(t, got.authOK)
	assert.Equal(t, "admin", got.user)
	assert.Equal(t, "secret", got.pass)
}

func TestRunRootOnlyFlagsRejected(t *testing.T) {
	path := writeRunbook(t, `
calls:
  health:
    path: _cluster/health
`)

	tests := []struct {
		name string
		args []string
	}{
		{name: "retry", args: []string{"run", path, "--retry", "3"}},
		{name: "path", args: []string{"run", path, "--path", "/x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := run(t, nil, tt.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unknown flag")
		})
	}
}

func TestRunVerbosePrintsPerAttemptLines(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusOK,
	})
	path := writeRunbook(t, `
calls:
  health:
    path: _cluster/health
    retry: 2
    backoff-initial: 1ms
`)

	_, stderr, err := run(t, nil, "run", path, "--endpoint", srv.URL, "-v")
	require.NoError(t, err)
	assert.Contains(t, stderr, "attempt 1")
	assert.Contains(t, stderr, "attempt 2")
	assert.Equal(t, 3, rec.len())
}

// run resolves its connection through a separate path from root
// (resolveConnection, not resolveConfig), so env fallback needs its own test.
func TestRunEnvVarFallback(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})
	path := writeRunbook(t, `
calls:
  health:
    path: _cluster/health
`)

	t.Setenv("OPENSEARCH_URL", srv.URL)
	t.Setenv("OPENSEARCH_USERNAME", "envuser")
	t.Setenv("OPENSEARCH_PASSWORD", "envpass")

	stdout, _, err := run(t, nil, "run", path)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	require.Equal(t, 1, rec.len())
	assert.Equal(t, "envuser", rec.at(0).user)
	assert.Equal(t, "envpass", rec.at(0).pass)
}

// A relative @file body resolves against the runbook's own directory, not the
// process working directory, so a runbook stays portable with its body files.
func TestRunRelativeFileBodyResolvesAgainstRunbookDir(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})

	path := writeRunbook(t, `
calls:
  index_doc:
    method: POST
    path: idx/_doc
    body: '@doc.json'
`)
	const body = `{"message":"hi"}`
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "doc.json"), []byte(body), 0o600))

	_, _, err := run(t, nil, "run", path, "--endpoint", srv.URL)
	require.NoError(t, err)
	require.Equal(t, 1, rec.len())
	assert.Equal(t, body, string(rec.at(0).body))
}
