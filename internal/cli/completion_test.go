package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// complete drives cobra's hidden __complete command and returns its raw output.
// cobra prints one candidate per line followed by a ":<directive>" trailer line.
func complete(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRootCommand("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"__complete"}, args...))
	require.NoError(t, root.ExecuteContext(context.Background()))
	return out.String()
}

func TestPathCompletion(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "prefix narrowing",
			args:        []string{"--path", "_clu"},
			wantContain: []string{"_cluster/"},
		},
		{
			name:        "segment descent",
			args:        []string{"--path", "_cluster/"},
			wantContain: []string{"_cluster/health"},
		},
		{
			name:        "leading slash mirrored",
			args:        []string{"--path", "/_clu"},
			wantContain: []string{"/_cluster/"},
		},
		{
			// Positive control for the filter case below: _aliases is POST-only,
			// so it must be offered when no method filter is applied.
			name:        "post-only path shown without method filter",
			args:        []string{"--path", "_aliases"},
			wantContain: []string{"_aliases"},
		},
		{
			name:       "method filter hides post-only path",
			args:       []string{"--method", "GET", "--path", "_aliases"},
			wantAbsent: []string{"_aliases"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := complete(t, tt.args...)
			for _, w := range tt.wantContain {
				assert.Contains(t, out, w)
			}
			for _, w := range tt.wantAbsent {
				assert.NotContains(t, out, w)
			}
		})
	}
}

func TestMethodCompletion(t *testing.T) {
	// Exact path match yields the endpoint's own methods.
	out := complete(t, "--path", "_cluster/health", "--method", "")
	assert.Contains(t, out, "GET")

	// Unknown path falls back to the standard verb list.
	out = complete(t, "--path", "definitely/not/a/real/path", "--method", "")
	assert.Contains(t, out, "DELETE")
}
