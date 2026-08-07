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

const runIndex = "e2e-run"

// TestRun_IndexLifecycle drives `osapi run` through a create-index/index-doc/
// refresh/wait-searchable runbook: four entry calls, the last gated by a
// check-only verify-with call. It covers depends-on chaining and the
// verify-with nested check together, and exercises @file bodies named
// relative to the runbook rather than to the process working directory.
func TestRun_IndexLifecycle(t *testing.T) {
	t.Cleanup(func() {
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", "/"+runIndex)...)
	})

	dir := t.TempDir()

	const docBodyFile, searchBodyFile = "doc-body.json", "search-body.json"
	require.NoError(t, os.WriteFile(filepath.Join(dir, docBodyFile),
		[]byte(`{"message":"hello-run-e2e"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, searchBodyFile),
		[]byte(`{"query":{"match_all":{}}}`), 0o600))

	runbookPath := filepath.Join(dir, "runbook.yaml")
	runbookYAML := fmt.Sprintf(`
calls:
  create_index:
    method: PUT
    path: /%[1]s/
    body: '{"settings":{"number_of_replicas":0}}'
    success-when: '.acknowledged'
    stop-on-failure: true

  index_doc:
    method: POST
    path: /%[1]s/_doc
    body: "@%[2]s"
    depends-on: create_index

  refresh:
    method: POST
    path: /%[1]s/_refresh
    depends-on: index_doc

  wait_searchable:
    method: POST
    path: /%[1]s/_search
    body: "@%[3]s"
    depends-on: refresh
    retry: 3
    verify-with: search_check

  search_check:
    method: POST
    path: /%[1]s/_search
    body: "@%[3]s"
    success-when: '.hits.total.value == 1'
`, runIndex, docBodyFile, searchBodyFile)
	require.NoError(t, os.WriteFile(runbookPath, []byte(runbookYAML), 0o600))

	res := runOsapi(t, nil, adminArgs("run", runbookPath)...)
	require.Equal(t, 0, res.exitCode, "run: stderr: %s", res.stderr)
	assert.Empty(t, res.stdout)
	assert.Contains(t, res.stderr, "run: 4 succeeded, 0 failed, 0 skipped")
}
