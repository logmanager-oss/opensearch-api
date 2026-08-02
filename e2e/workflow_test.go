//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const workflowIndex = "e2e-workflow"

// TestWorkflow_IndexLifecycle drives a full create-index/index-doc/refresh/
// search/delete-index cycle as admin, covering the --body @file mode along
// the way (the search query is passed via -d @<tempfile>).
func TestWorkflow_IndexLifecycle(t *testing.T) {
	t.Cleanup(func() {
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", "/"+workflowIndex)...)
	})

	res := runOsapi(t, nil, adminArgs("--path", "/_cluster/health")...)
	require.Equal(t, 0, res.exitCode, "cluster health: stderr: %s", res.stderr)

	res = runOsapi(t, nil, adminArgs(
		"-X", "PUT", "--path", "/"+workflowIndex,
		"-d", `{"settings":{"number_of_replicas":0}}`,
		"--success-when", ".acknowledged")...)
	require.Equal(t, 0, res.exitCode, "creating index: stderr: %s", res.stderr)
	require.Contains(t, res.stdout, `"acknowledged":true`)

	res = runOsapi(t, nil, adminArgs(
		"-X", "POST", "--path", "/"+workflowIndex+"/_doc",
		"-d", `{"message":"hello-e2e"}`)...)
	require.Equal(t, 0, res.exitCode, "indexing doc: stderr: %s", res.stderr)

	res = runOsapi(t, nil, adminArgs("-X", "POST", "--path", "/"+workflowIndex+"/_refresh")...)
	require.Equal(t, 0, res.exitCode, "refreshing index: stderr: %s", res.stderr)

	queryPath := filepath.Join(t.TempDir(), "query.json")
	require.NoError(t, os.WriteFile(queryPath,
		[]byte(`{"query":{"match":{"message":"hello-e2e"}}}`), 0o600))

	res = runOsapi(t, nil, adminArgs(
		"-X", "POST", "--path", "/"+workflowIndex+"/_search", "-d", "@"+queryPath)...)
	require.Equal(t, 0, res.exitCode, "searching: stderr: %s", res.stderr)
	assert.Contains(t, res.stdout, "hello-e2e")
	assert.Contains(t, res.stdout, `"value":1`)

	res = runOsapi(t, nil, adminArgs("-X", "DELETE", "--path", "/"+workflowIndex)...)
	require.Equal(t, 0, res.exitCode, "deleting index: stderr: %s", res.stderr)
}
