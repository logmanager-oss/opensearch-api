//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rbacIndex = "e2e-rbac"

// setupRBACFixture creates rbacIndex as admin with one immediately-searchable
// document so ci_reader/ci_locked read tests have something to see.
// t.Cleanup deletes the index, ignoring errors (it may already be gone).
func setupRBACFixture(t *testing.T) {
	t.Helper()

	// Registered before the creates so a mid-fixture failure cannot leak the
	// index and poison `make e2e-test` reruns against the same stack.
	t.Cleanup(func() {
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", "/"+rbacIndex)...)
	})

	res := runOsapi(t, nil, adminArgs(
		"-X", "PUT", "--path", "/"+rbacIndex,
		"-d", `{"settings":{"number_of_shards":1,"number_of_replicas":0}}`)...)
	require.Equal(t, 0, res.exitCode, "creating index: stderr: %s", res.stderr)

	res = runOsapi(t, nil, adminArgs(
		"-X", "POST", "--path", "/"+rbacIndex+"/_doc",
		"-q", "refresh=true", "-d", `{"message":"hello"}`)...)
	require.Equal(t, 0, res.exitCode, "indexing doc: stderr: %s", res.stderr)
}

func TestRBAC_ReaderAllowedReads(t *testing.T) {
	setupRBACFixture(t)

	tests := []struct {
		name string
		path string
	}{
		{name: "cluster health", path: "/_cluster/health"},
		{name: "search the fixture index", path: "/" + rbacIndex + "/_search"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runOsapi(t, nil, asUser(readerUser, readerPass, "--path", tt.path)...)
			assert.Equal(t, 0, res.exitCode, "stderr: %s", res.stderr)
		})
	}
}

func TestRBAC_ReaderForbiddenWrite(t *testing.T) {
	res := runOsapi(t, nil, asUser(readerUser, readerPass,
		"-X", "PUT", "--path", "/e2e-rbac-denied")...)
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stdout, "security_exception")
	assert.Contains(t, res.stdout, "403")
	assert.Contains(t, res.stderr, "retries exhausted")
}

func TestRBAC_LockedUserForbiddenEverywhere(t *testing.T) {
	res := runOsapi(t, nil, asUser(lockedUser, lockedPass, "--path", "/_cluster/health")...)
	require.Equal(t, 1, res.exitCode)
	assert.Contains(t, res.stdout, "security_exception")
	assert.Contains(t, res.stdout, "403")
}
