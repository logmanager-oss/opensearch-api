package apispec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSuggest(t *testing.T) {
	tests := []struct {
		name        string
		typed       string
		method      string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "prefix narrowing",
			typed:       "_clu",
			wantContain: []string{"_cluster/"},
			wantAbsent:  []string{"/_cluster/", "_cluster/health"},
		},
		{
			name:        "segment descent surfaces terminal and child",
			typed:       "_cluster/",
			wantContain: []string{"_cluster/health", "_cluster/health/"},
		},
		{
			name:        "param placeholder is terminal hint",
			typed:       "_cluster/health/",
			wantContain: []string{"_cluster/health/{index}"},
			wantAbsent:  []string{"_cluster/health/{index}/"},
		},
		{
			// A typed leading slash is mirrored back so candidates survive the
			// shell's literal-prefix filter; the slash-less form is not offered.
			name:        "leading slash mirrored in candidates",
			typed:       "/_clu",
			wantContain: []string{"/_cluster/"},
			wantAbsent:  []string{"_cluster/"},
		},
		{
			name:       "method filter hides post-only path",
			typed:      "_aliases",
			method:     "GET",
			wantAbsent: []string{"_aliases"},
		},
		{
			name:        "method filter keeps matching path",
			typed:       "_aliases",
			method:      "POST",
			wantContain: []string{"_aliases"},
		},
		{
			name:        "no method considers all",
			typed:       "_aliases",
			wantContain: []string{"_aliases"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Suggest(tt.typed, tt.method)
			for _, w := range tt.wantContain {
				assert.Contains(t, got, w)
			}
			for _, w := range tt.wantAbsent {
				assert.NotContains(t, got, w)
			}
			assert.IsIncreasing(t, got)
		})
	}
}

func TestMethodsFor(t *testing.T) {
	tests := []struct {
		name  string
		typed string
		want  []string
	}{
		{name: "exact match", typed: "_cluster/health", want: []string{"GET"}},
		{name: "exact match leading slash", typed: "/_cluster/health", want: []string{"GET"}},
		{name: "exact match multi method", typed: "{index}/_search", want: []string{"GET", "POST"}},
		{name: "no match returns nil", typed: "_cluster/", want: nil},
		{name: "partial is no match", typed: "_clu", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MethodsFor(tt.typed))
		})
	}
}
