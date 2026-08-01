//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeDotenv writes a dotenv file in t.TempDir() with the given lines and
// returns its path.
func writeDotenv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "e2e.env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestConfig_ProcessEnvProvidesConnection(t *testing.T) {
	env := map[string]string{
		"OPENSEARCH_URL":      baseURL,
		"OPENSEARCH_USERNAME": adminUser,
		"OPENSEARCH_PASSWORD": adminPass,
	}
	res := runOsapi(t, env, "--ca-cert", caCertPath, "--path", "/_cluster/health")
	require.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
}

func TestConfig_Precedence(t *testing.T) {
	dotenv := func(pass string) string {
		return "OPENSEARCH_URL=" + baseURL +
			"\nOPENSEARCH_USERNAME=" + adminUser +
			"\nOPENSEARCH_PASSWORD=" + pass + "\n"
	}

	t.Run("env-file beats process env", func(t *testing.T) {
		path := writeDotenv(t, dotenv(adminPass))
		env := map[string]string{"OPENSEARCH_PASSWORD": "wrong-password"}
		res := runOsapi(t, env,
			"--env-file", path, "--ca-cert", caCertPath, "--path", "/_cluster/health")
		assert.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
	})

	t.Run("env-file beats process env even when env-file is wrong", func(t *testing.T) {
		path := writeDotenv(t, dotenv("wrong-password"))
		env := map[string]string{
			"OPENSEARCH_URL":      baseURL,
			"OPENSEARCH_USERNAME": adminUser,
			"OPENSEARCH_PASSWORD": adminPass,
		}
		res := runOsapi(t, env,
			"--env-file", path, "--ca-cert", caCertPath, "--path", "/_cluster/health")
		require.Equal(t, 1, res.exitCode)
		assert.Contains(t, res.stdout, "Unauthorized")
	})

	t.Run("--password flag beats env-file", func(t *testing.T) {
		path := writeDotenv(t, dotenv("wrong-password"))
		res := runOsapi(t, nil,
			"--env-file", path, "--password", adminPass,
			"--ca-cert", caCertPath, "--path", "/_cluster/health")
		assert.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
	})

	t.Run("--endpoint flag beats OPENSEARCH_URL env", func(t *testing.T) {
		env := map[string]string{
			"OPENSEARCH_URL":      "https://127.0.0.1:1",
			"OPENSEARCH_USERNAME": adminUser,
			"OPENSEARCH_PASSWORD": adminPass,
		}
		res := runOsapi(t, env,
			"--endpoint", baseURL, "--ca-cert", caCertPath, "--path", "/_cluster/health")
		assert.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
	})
}
