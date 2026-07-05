package apispec

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPathsNonEmpty(t *testing.T) {
	require.NotEmpty(t, Paths)
}

func TestPathsContainsKnownTemplates(t *testing.T) {
	want := []string{
		"/_cluster/health",
		"/_plugins/_ism/policies/{policy_id}",
	}
	got := make(map[string]bool, len(Paths))
	for _, ep := range Paths {
		got[ep.Template] = true
	}
	for _, tmpl := range want {
		assert.True(t, got[tmpl], "expected template %q", tmpl)
	}
}

func TestEndpointMethodsValid(t *testing.T) {
	for _, ep := range Paths {
		require.NotEmpty(t, ep.Methods, "template %q has no methods", ep.Template)
		for _, m := range ep.Methods {
			assert.Equal(t, m, strings.ToUpper(m), "method %q not uppercase", m)
		}
	}
}
