package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionFlag(t *testing.T) {
	root := NewRootCommand("1.2.3")
	var out, errb bytes.Buffer
	root.SetArgs([]string{"--version"})
	root.SetOut(&out)
	root.SetErr(&errb)

	require.NoError(t, root.ExecuteContext(context.Background()))
	assert.Contains(t, out.String(), "1.2.3")
	assert.Empty(t, errb.String())
	// --version short-circuits: no required-flag (--path) error.
	assert.NotContains(t, strings.ToLower(out.String()), "required")
}
