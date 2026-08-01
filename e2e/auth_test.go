//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuth_ValidCredentials(t *testing.T) {
	res := runOsapi(t, nil, adminArgs("--path", "_cluster/health")...)
	require.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
	assert.True(t, json.Valid([]byte(res.stdout)), "stdout must be valid JSON: %s", res.stdout)
}

func TestAuth_WrongPassword(t *testing.T) {
	res := runOsapi(t, nil, asUser(adminUser, "wrong-password", "--path", "_cluster/health")...)
	require.Equal(t, 1, res.exitCode)
	// The security plugin's 401 body is plain text, not JSON.
	assert.Contains(t, res.stdout, "Unauthorized")
	assert.Contains(t, res.stderr, "retries exhausted")
}

func TestAuth_NoCredentials(t *testing.T) {
	res := runOsapi(t, nil,
		"--endpoint", baseURL, "--ca-cert", caCertPath, "--path", "_cluster/health")
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stdout, "Unauthorized")
}

func TestAuth_NonTTYMissingPassword(t *testing.T) {
	res := runOsapi(t, nil,
		"--endpoint", baseURL, "-u", adminUser, "--ca-cert", caCertPath, "--path", "_cluster/health")
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stderr, "no password")
	assert.Empty(t, res.stdout)
}
