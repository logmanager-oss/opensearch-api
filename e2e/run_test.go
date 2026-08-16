//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const runbookIndex = "e2e-runbook"

// TestRunbook_IndexLifecycle runs `osapi run` end to end, covering defaults:
// retry, success-when on the idempotent delete, an @file body, and ${}
// capture substitution.
func TestRunbook_IndexLifecycle(t *testing.T) {
	t.Cleanup(func() {
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", "/"+runbookIndex)...)
	})

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index-settings.json"),
		[]byte(`{"settings":{"number_of_replicas":0}}`), 0o600))

	runbookPath := filepath.Join(dir, "runbook.yaml")
	require.NoError(t, os.WriteFile(runbookPath, []byte(fmt.Sprintf(`
defaults:
  retry: 2
  backoff: constant
  backoff-initial: '200ms'

calls:
  - name: delete_stale
    method: DELETE
    path: /%[1]s
    success-when: '.acknowledged == true or .status == 404'

  - name: create_index
    method: PUT
    path: /%[1]s
    body: '@index-settings.json'
    success-when: '.acknowledged'

  - name: index_doc
    method: PUT
    path: /%[1]s/_doc/1
    body: '{"message":"hello-runbook"}'

  - name: refresh
    method: POST
    path: /%[1]s/_refresh

  - name: get_doc
    method: GET
    path: /%[1]s/_doc/1
    capture:
      seq: '._seq_no'
      term: '._primary_term'

  - name: update_doc
    method: PUT
    path: /%[1]s/_doc/1
    query:
      if_seq_no: '${seq}'
      if_primary_term: '${term}'
    body: '{"message":"updated-runbook"}'

  - name: search
    method: POST
    path: /%[1]s/_search
    retry: 3
    success-when: '.hits.total.value == 1'
    body: '{"query":{"match_all":{}}}'
`, runbookIndex)), 0o600))

	res := runOsapi(t, nil, adminArgs("run", runbookPath)...)
	require.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
	assert.Empty(t, res.stdout)
	assert.Contains(t, res.stderr, "run: 7 succeeded")
}

// TestRunbook_HaltedRunReportsNotRun checks a 404 with no success-when,
// retry, or abort-on halts the run: exit 1, the failing body on stderr, and
// the trailing call marked not run.
func TestRunbook_HaltedRunReportsNotRun(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "runbook.yaml")
	require.NoError(t, os.WriteFile(runbookPath, []byte(`
calls:
  - name: ok_call
    path: /_cluster/health
  - name: bad_call
    path: /e2e-runbook-does-not-exist
  - name: never_run
    path: /_cluster/health
`), 0o600))

	res := runOsapi(t, nil, adminArgs("run", runbookPath)...)
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stderr, `call "ok_call": ok`)
	assert.Contains(t, res.stderr, "index_not_found_exception", "the failing call's body is echoed on stderr")
	assert.Contains(t, res.stderr, "run: 1 succeeded, 1 failed (halted), 1 not run")
}

// TestRunbook_DryRunSendsNothing checks --dry-run against a real runbook:
// exit 0, empty stdout, and no request sent. Proven against the live
// cluster, not a mock.
func TestRunbook_DryRunSendsNothing(t *testing.T) {
	const dryRunIndex = "e2e-runbook-dryrun"
	t.Cleanup(func() {
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", "/"+dryRunIndex)...)
	})

	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "runbook.yaml")
	require.NoError(t, os.WriteFile(runbookPath, []byte(fmt.Sprintf(`
calls:
  - name: create_dryrun_index
    method: PUT
    path: /%s
    success-when: '.acknowledged'
`, dryRunIndex)), 0o600))

	res := runOsapi(t, nil, adminArgs("run", runbookPath, "--dry-run")...)
	require.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
	assert.Empty(t, res.stdout)
	assert.Contains(t, res.stderr, "dry-run: 1 call(s), no requests sent")

	// Nothing was sent: the index the plan describes was never created.
	check := runOsapi(t, nil, adminArgs("--path", "/"+dryRunIndex)...)
	require.Equal(t, 1, check.exitCode)
	assert.Contains(t, check.stdout, "index_not_found_exception")
}

// Creating an index needs admin rights, so success as ci_reader proves the
// runbook's credentials, not asUser's -u/--password flags, were used.
func TestRunbook_CredentialsOverrideFlags(t *testing.T) {
	const idx = "e2e-runbook-creds-override"
	t.Cleanup(func() {
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", "/"+idx)...)
	})

	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "runbook.yaml")
	require.NoError(t, os.WriteFile(runbookPath, []byte(fmt.Sprintf(`
defaults:
  credentials:
    username: '%s'
    password: '%s'
calls:
  - name: create_index
    method: PUT
    path: /%s
    success-when: '.acknowledged'
`, adminUser, adminPass, idx)), 0o600))

	res := runOsapi(t, nil, asUser(readerUser, readerPass, "run", runbookPath)...)
	require.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
	assert.Contains(t, res.stderr, "run: 1 succeeded")
}
