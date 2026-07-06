package apispec

import (
	"encoding/json"
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

func TestMatchTemplate(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{name: "exact literal", path: "_cluster/health", want: "/_cluster/health", wantOK: true},
		{name: "leading slash accepted", path: "/_cluster/health", want: "/_cluster/health", wantOK: true},
		{name: "param substitution", path: "myindex/_search", want: "/{index}/_search", wantOK: true},
		{name: "fewest params wins", path: "_search", want: "/_search", wantOK: true},
		{name: "no match", path: "not/a/real/path/here", wantOK: false},
		{name: "empty path no match", path: "", wantOK: false},
		{name: "root slash no match", path: "/", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MatchTemplate(tt.path)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestBodySkeleton(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		method   string
		wantOK   bool
		wantJSON string // exact, when set
		contains []string
		isObject bool
	}{
		{name: "object body with typed fields", path: "_search", method: "POST", wantOK: true, contains: []string{`"query": {}`, `"size": 0`}, isObject: true},
		{name: "concrete index maps to template", path: "myindex/_search", method: "post", wantOK: true, contains: []string{`"query": {}`}, isObject: true},
		{name: "array body not scaffolded", path: "_bulk", method: "POST", wantOK: false},
		{name: "no documented body", path: "_cluster/health", method: "GET", wantOK: false},
		{name: "unmatched path", path: "not/a/real/path", method: "POST", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := BodySkeleton(tt.path, tt.method)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			assert.True(t, json.Valid([]byte(got)), "output must be valid JSON: %s", got)
			if tt.wantJSON != "" {
				assert.Equal(t, tt.wantJSON, got)
			}
			for _, sub := range tt.contains {
				assert.Contains(t, got, sub)
			}
			if tt.isObject {
				var m map[string]any
				assert.NoError(t, json.Unmarshal([]byte(got), &m))
			}
		})
	}
}
