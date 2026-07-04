package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func mapLookup(m map[string]string) EnvLookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestEnvEndpoint(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		want  string
		found bool
	}{
		{name: "set", env: map[string]string{"OPENSEARCH_URL": "https://b"}, want: "https://b", found: true},
		{name: "empty treated as unset", env: map[string]string{"OPENSEARCH_URL": ""}, found: false},
		{name: "none", env: map[string]string{}, found: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := envEndpoint(mapLookup(tt.env))
			assert.Equal(t, tt.found, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEnvUsername(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		want  string
		found bool
	}{
		{name: "set", env: map[string]string{"OPENSEARCH_USERNAME": "b"}, want: "b", found: true},
		{name: "empty treated as unset", env: map[string]string{"OPENSEARCH_USERNAME": ""}, found: false},
		{name: "none", env: map[string]string{}, found: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := envUsername(mapLookup(tt.env))
			assert.Equal(t, tt.found, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEnvPassword(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		want  string
		found bool
	}{
		{name: "set", env: map[string]string{"OPENSEARCH_PASSWORD": "b"}, want: "b", found: true},
		{name: "empty treated as unset", env: map[string]string{"OPENSEARCH_PASSWORD": ""}, found: false},
		{name: "none", env: map[string]string{}, found: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := envPassword(mapLookup(tt.env))
			assert.Equal(t, tt.found, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEnvLookupNil(t *testing.T) {
	_, ok := envEndpoint(nil)
	assert.False(t, ok)
	_, ok = envUsername(nil)
	assert.False(t, ok)
	_, ok = envPassword(nil)
	assert.False(t, ok)
}
