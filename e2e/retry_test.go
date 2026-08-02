//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These run as admin: as ci_reader a missing index can surface as 403 instead
// of 404, which would break the stable-404 assumption both tests rely on.

func TestRetry_ExhaustsOnStable404(t *testing.T) {
	res := runOsapi(t, nil, adminArgs(
		"--path", "/e2e-no-such-index",
		"--retry", "2", "--backoff", "constant", "--backoff-initial", "10ms", "-v")...)
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stderr, "attempt 1: status 404")
	assert.Contains(t, res.stderr, "attempt 2: status 404")
	assert.Contains(t, res.stderr, "after 3 attempts: retries exhausted")
	assert.Contains(t, res.stdout, "index_not_found_exception")
}

func TestRetry_AbortOnStopsImmediately(t *testing.T) {
	res := runOsapi(t, nil, adminArgs(
		"--path", "/e2e-no-such-index",
		"--retry", "5", "--abort-on", "404", "-v")...)
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stderr, "terminal status 404")
	assert.NotContains(t, res.stderr, "attempt 1")
}

func TestRetry_SuccessWhenNeverSatisfiedExhausts(t *testing.T) {
	res := runOsapi(t, nil, adminArgs(
		"--path", "/_cluster/health",
		"--retry", "1", "--backoff", "constant", "--backoff-initial", "10ms",
		"--success-when", `.status == "nonexistent"`)...)
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stderr, "retries exhausted: --success-when not satisfied")
}
