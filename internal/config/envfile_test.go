package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestLoadEnvFile is a smoke test that the wrapper delegates to godotenv
// correctly; exhaustive dotenv parsing is godotenv's responsibility.
func TestLoadEnvFile(t *testing.T) {
	path := writeEnvFile(t,
		"# a comment\n"+
			"\n"+
			"OPENSEARCH_URL=https://os:9200\n"+
			"export OPENSEARCH_USERNAME=admin\n"+
			"OPENSEARCH_PASSWORD=\"quoted value\"\n"+
			"EMPTY=\n")

	got, err := LoadEnvFile(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"OPENSEARCH_URL":      "https://os:9200",
		"OPENSEARCH_USERNAME": "admin",
		"OPENSEARCH_PASSWORD": "quoted value",
		"EMPTY":               "",
	}, got)
}

func TestLoadEnvFileMalformed(t *testing.T) {
	_, err := LoadEnvFile(writeEnvFile(t, "OPENSEARCH_URL=https://os:9200\nSECRETLINEVALUE\n"))
	require.Error(t, err)
	// The error must not echo the offending line, so a secret on a malformed
	// line never leaks (e.g. into CI logs).
	assert.NotContains(t, err.Error(), "SECRETLINEVALUE")
}

func TestLoadEnvFileMissing(t *testing.T) {
	_, err := LoadEnvFile(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
}

func TestLayeredEnv(t *testing.T) {
	base := mapLookup(map[string]string{
		"FROM_BASE":     "base",
		"OVERRIDE":      "base",
		"EMPTY_IN_FILE": "base",
	})
	file := map[string]string{
		"OVERRIDE":      "file",
		"EMPTY_IN_FILE": "",
		"FILE_ONLY":     "file",
	}
	env := LayeredEnv(file, base)

	t.Run("file value wins over base", func(t *testing.T) {
		v, ok := env("OVERRIDE")
		assert.True(t, ok)
		assert.Equal(t, "file", v)
	})

	t.Run("missing key falls back to base", func(t *testing.T) {
		v, ok := env("FROM_BASE")
		assert.True(t, ok)
		assert.Equal(t, "base", v)
	})

	t.Run("empty file value falls through to base", func(t *testing.T) {
		v, ok := env("EMPTY_IN_FILE")
		assert.True(t, ok)
		assert.Equal(t, "base", v)
	})

	t.Run("file-only key resolves", func(t *testing.T) {
		v, ok := env("FILE_ONLY")
		assert.True(t, ok)
		assert.Equal(t, "file", v)
	})

	t.Run("unknown key unset", func(t *testing.T) {
		_, ok := env("UNKNOWN")
		assert.False(t, ok)
	})
}

func TestLayeredEnvNilBase(t *testing.T) {
	env := LayeredEnv(map[string]string{"FILE_ONLY": "file"}, nil)

	v, ok := env("FILE_ONLY")
	assert.True(t, ok)
	assert.Equal(t, "file", v)

	_, ok = env("MISSING")
	assert.False(t, ok)
}
