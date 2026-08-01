//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLS_InsecureFlagWorks(t *testing.T) {
	res := runOsapi(t, nil,
		"--endpoint", baseURL, "-u", adminUser, "--password", adminPass, "-k",
		"--path", "_cluster/health")
	require.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
	assert.Contains(t, res.stdout, `"cluster_name"`)
}

func TestTLS_SystemRootsRejected(t *testing.T) {
	res := runOsapi(t, nil,
		"--endpoint", baseURL, "-u", adminUser, "--password", adminPass,
		"--path", "_cluster/health")
	require.Equal(t, 1, res.exitCode)
	assert.Empty(t, res.stdout)
	assert.Contains(t, res.stderr, "x509")
	assert.Contains(t, res.stderr, "retries exhausted")
}

func TestTLS_CACertWorks(t *testing.T) {
	res := runOsapi(t, nil, adminArgs("--path", "_cluster/health")...)
	require.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
}

func TestTLS_WrongCAFails(t *testing.T) {
	res := runOsapi(t, nil,
		"--endpoint", baseURL, "-u", adminUser, "--password", adminPass,
		"--ca-cert", wrongCACertPath, "--path", "_cluster/health")
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stderr, "x509")
}
