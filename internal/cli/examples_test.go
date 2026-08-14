package cli

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExamplesDryRun keeps every runbook in examples/ loadable: --dry-run
// exercises the full load/validate path with no endpoint, credentials or
// network.
func TestExamplesDryRun(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no runbooks found in examples/")

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			stdout, stderr, err := run(t, nil, "run", "--dry-run", file)
			require.NoError(t, err)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "no requests sent")
		})
	}
}
