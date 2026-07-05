package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// changedSet builds a Changed predicate from a set of field names.
func changedSet(fields ...string) func(string) bool {
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	return func(field string) bool { return set[field] }
}

func TestResolveDefaults(t *testing.T) {
	got, err := Resolve(Sources{})
	require.NoError(t, err)
	assert.Equal(t, Defaults(), got)
}

func TestResolveEndpointPrecedence(t *testing.T) {
	env := mapLookup(map[string]string{"OPENSEARCH_URL": "https://env"})

	tests := []struct {
		name    string
		sources Sources
		want    string
	}{
		{
			name: "flag beats env",
			sources: Sources{
				Flags:   Config{Endpoint: "https://flag"},
				Changed: changedSet(FieldEndpoint),
				Env:     env,
			},
			want: "https://flag",
		},
		{
			name:    "env beats default",
			sources: Sources{Env: env},
			want:    "https://env",
		},
		{
			name:    "default when nothing set",
			sources: Sources{},
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.sources)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Endpoint)
		})
	}
}

func TestResolveUsernameAndPassword(t *testing.T) {
	env := mapLookup(map[string]string{
		"OPENSEARCH_USERNAME": "envuser",
		"OPENSEARCH_PASSWORD": "envpass",
	})

	t.Run("username and password from env", func(t *testing.T) {
		got, err := Resolve(Sources{Env: env})
		require.NoError(t, err)
		assert.Equal(t, "envuser", got.Username)
		assert.Equal(t, "envpass", got.Password)
	})

	t.Run("flag username beats env", func(t *testing.T) {
		got, err := Resolve(Sources{
			Flags:   Config{Username: "flaguser"},
			Changed: changedSet(FieldUsername),
			Env:     env,
		})
		require.NoError(t, err)
		assert.Equal(t, "flaguser", got.Username)
		assert.Equal(t, "envpass", got.Password)
	})

	t.Run("flag password beats env", func(t *testing.T) {
		got, err := Resolve(Sources{
			Flags:   Config{Password: "flagpass"},
			Changed: changedSet(FieldPassword),
			Env:     env,
		})
		require.NoError(t, err)
		assert.Equal(t, "flagpass", got.Password)
	})
}

func TestResolveEmptyEnvTreatedAsUnset(t *testing.T) {
	env := mapLookup(map[string]string{
		"OPENSEARCH_URL":      "",
		"OPENSEARCH_USERNAME": "",
		"OPENSEARCH_PASSWORD": "",
	})

	got, err := Resolve(Sources{Env: env})
	require.NoError(t, err)
	assert.Empty(t, got.Endpoint)
	assert.Empty(t, got.Username)
	assert.Empty(t, got.Password)
}

func TestResolveNilEnv(t *testing.T) {
	got, err := Resolve(Sources{Env: nil})
	require.NoError(t, err)
	assert.Equal(t, Defaults(), got)
}

func TestResolveInsecureAndCACert(t *testing.T) {
	got, err := Resolve(Sources{
		Flags:   Config{Insecure: true, CACertPath: "/flag/ca.pem"},
		Changed: changedSet(FieldInsecure, FieldCACert),
	})
	require.NoError(t, err)
	assert.True(t, got.Insecure)
	assert.Equal(t, "/flag/ca.pem", got.CACertPath)
}

func TestResolveRetryFlags(t *testing.T) {
	flags := Defaults()
	flags.Retry.MaxRetries = 3
	flags.Retry.Strategy = Constant
	flags.Retry.Initial = 500 * time.Millisecond
	flags.Retry.Max = 10 * time.Second
	flags.Retry.Jitter = 0.5
	flags.Retry.AbortOn = []int{400, 409}

	got, err := Resolve(Sources{
		Flags: flags,
		Changed: changedSet(FieldRetry, FieldBackoff, FieldBackoffInitial,
			FieldBackoffMax, FieldBackoffJitter, FieldAbortOn),
	})
	require.NoError(t, err)
	assert.Equal(t, 3, got.Retry.MaxRetries)
	assert.Equal(t, Constant, got.Retry.Strategy)
	assert.Equal(t, 500*time.Millisecond, got.Retry.Initial)
	assert.Equal(t, 10*time.Second, got.Retry.Max)
	assert.InDelta(t, 0.5, got.Retry.Jitter, 1e-9)
	assert.Equal(t, []int{400, 409}, got.Retry.AbortOn)
}

func TestResolveClonesSlices(t *testing.T) {
	src := []int{409}
	flags := Defaults()
	flags.Retry.AbortOn = src

	got, err := Resolve(Sources{
		Flags:   flags,
		Changed: changedSet(FieldAbortOn),
	})
	require.NoError(t, err)
	require.Equal(t, []int{409}, got.Retry.AbortOn)

	got.Retry.AbortOn[0] = 999
	assert.Equal(t, 409, src[0], "resolved slice must not alias the source")
}

// TestResolveWithLayeredEnvFile wires the full stack end-to-end: an env file
// layered over the process environment feeds Resolve, proving
// file > process-env > default for endpoint, username and password.
func TestResolveWithLayeredEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(
		"OPENSEARCH_URL=https://file\n"+
			"OPENSEARCH_PASSWORD=filepass\n"), 0o600))

	fileVars, err := LoadEnvFile(path)
	require.NoError(t, err)

	processEnv := mapLookup(map[string]string{
		"OPENSEARCH_URL":      "https://process",
		"OPENSEARCH_USERNAME": "processuser",
	})

	got, err := Resolve(Sources{Env: LayeredEnv(fileVars, processEnv)})
	require.NoError(t, err)

	assert.Equal(t, "https://file", got.Endpoint) // file wins over process-env
	assert.Equal(t, "processuser", got.Username)  // process-env fills the gap
	assert.Equal(t, "filepass", got.Password)     // file-only value
}
