//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ismPolicyID   = "e2e-ism"
	ismPolicyPath = "_plugins/_ism/policies/" + ismPolicyID
	ismIndex      = "e2e-ism-000001"
	ismConflictID = "e2e-ism-conflict"
	ismFixture    = "../examples/ism-policy.json"
)

// ismGet returns the stored policy's GET body and its _seq_no.
func ismGet(t *testing.T) (body string, seqNo int) {
	t.Helper()
	res := runOsapi(t, nil, adminArgs("--path", ismPolicyPath)...)
	require.Equal(t, 0, res.exitCode, "get policy: stderr: %s", res.stderr)
	var doc struct {
		SeqNo int `json:"_seq_no"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.stdout), &doc), res.stdout)
	return res.stdout, doc.SeqNo
}

// ismManaged polls explain until it reports ismIndex managed by ismPolicyID,
// or not, as want says. ISM records managed indices asynchronously.
func ismManaged(t *testing.T, want bool) {
	t.Helper()
	var last string
	ok := assert.Eventually(t, func() bool {
		res, err := execOsapi(nil, adminArgs("--path", "_plugins/_ism/explain/"+ismIndex)...)
		last = res.stdout
		return err == nil && res.exitCode == 0 &&
			strings.Contains(res.stdout, `"policy_id":"`+ismPolicyID+`"`) == want
	}, 60*time.Second, time.Second)
	require.True(t, ok, "explain managed=%v never reached: %s", want, last)
}

func requireNoneManaged(t *testing.T, res result) {
	t.Helper()
	require.Equal(t, 0, res.exitCode, "reapply none managed: stderr: %s", res.stderr)
	assert.Contains(t, res.stderr, "no indices are managed by "+ismPolicyID)
	assert.Empty(t, strings.TrimSpace(res.stdout))
}

func TestISM_PolicyLifecycle(t *testing.T) {
	t.Cleanup(func() {
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", "/"+ismIndex)...)
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", ismPolicyPath)...)
	})

	fixture, err := os.ReadFile(ismFixture)
	require.NoError(t, err)
	var changed map[string]any
	require.NoError(t, json.Unmarshal(fixture, &changed))
	changed["policy"].(map[string]any)["description"] = "changed by e2e"
	changedBytes, err := json.Marshal(changed)
	require.NoError(t, err)
	dir := t.TempDir()
	changedPath := filepath.Join(dir, "changed.json")
	require.NoError(t, os.WriteFile(changedPath, changedBytes, 0o600))

	setArgs := func(file string, extra ...string) []string {
		return adminArgs(append([]string{"ism", "set", ismPolicyID, "-d", "@" + file,
			"--target", "e2e-ism-*", "--priority", "50"}, extra...)...)
	}

	res := runOsapi(t, nil, setArgs(ismFixture)...)
	require.Equal(t, 0, res.exitCode, "set create: stderr: %s", res.stderr)
	_, seq := ismGet(t)

	requireNoneManaged(t, runOsapi(t, nil, adminArgs("ism", "reapply", ismPolicyID)...))

	// The example file omits defaults ISM stores (error_notification, per-action retry), so it compares as changed.
	res = runOsapi(t, nil, setArgs(ismFixture)...)
	require.Equal(t, 0, res.exitCode, "set same file: stderr: %s", res.stderr)
	require.NotContains(t, res.stderr, "unchanged")
	_, same := ismGet(t)
	require.Greater(t, same, seq, "a file that omits ISM defaults is PUT again")

	res = runOsapi(t, nil, setArgs(changedPath)...)
	require.Equal(t, 0, res.exitCode, "set changed: stderr: %s", res.stderr)
	require.NotContains(t, res.stderr, "unchanged")
	stored, before := ismGet(t)
	require.Greater(t, before, same, "changed body must PUT through CAS")

	storedPath := filepath.Join(dir, "stored.json")
	require.NoError(t, os.WriteFile(storedPath, []byte(stored), 0o600))
	res = runOsapi(t, nil, adminArgs("ism", "set", ismPolicyID, "-d", "@"+storedPath)...)
	require.Equal(t, 0, res.exitCode, "round trip: stderr: %s", res.stderr)
	require.Contains(t, res.stderr, "unchanged", "round trip: stdout: %s", res.stdout)
	_, after := ismGet(t)
	require.Equal(t, before, after, "unchanged set must not PUT")

	res = runOsapi(t, nil, adminArgs("ism", "set", ismPolicyID, "-d", "@"+storedPath, "--force")...)
	require.Equal(t, 0, res.exitCode, "set force: stderr: %s", res.stderr)
	require.NotContains(t, res.stderr, "unchanged")
	_, forced := ismGet(t)
	require.Greater(t, forced, after, "--force must PUT")

	res = runOsapi(t, nil, adminArgs("-X", "PUT", "--path", "/"+ismIndex,
		"-d", `{"settings":{"number_of_replicas":0}}`)...)
	require.Equal(t, 0, res.exitCode, "create index: stderr: %s", res.stderr)
	ismManaged(t, true)

	res = runOsapi(t, nil, adminArgs("ism", "reapply", ismPolicyID)...)
	require.Equal(t, 0, res.exitCode, "reapply: stderr: %s", res.stderr)
	require.Contains(t, res.stdout, `"updated_indices":1`, res.stdout)

	res = runOsapi(t, nil, adminArgs("ism", "reapply", ismPolicyID, "--target", "e2e-ism-*")...)
	require.Equal(t, 0, res.exitCode, "reapply target: stderr: %s", res.stderr)
	require.Contains(t, res.stdout, `"updated_indices":1`, res.stdout)

	// On 3.9, deleting the policy does not detach the index: explain still reports the policy_id. Detach explicitly.
	res = runOsapi(t, nil, adminArgs("-X", "POST", "--path", "_plugins/_ism/remove/"+ismIndex)...)
	require.Equal(t, 0, res.exitCode, "ism remove: stderr: %s", res.stderr)
	ismManaged(t, false)
	requireNoneManaged(t, runOsapi(t, nil, adminArgs("ism", "reapply", ismPolicyID)...))
}

func TestISM_UnconditionalPutConflicts(t *testing.T) {
	t.Cleanup(func() {
		_, _ = execOsapi(nil, adminArgs("-X", "DELETE", "--path", "_plugins/_ism/policies/"+ismConflictID)...)
	})

	put := adminArgs("-X", "PUT", "--path", "_plugins/_ism/policies/"+ismConflictID, "-d", "@"+ismFixture)

	res := runOsapi(t, nil, put...)
	require.Equal(t, 0, res.exitCode, "plain put create: stderr: %s", res.stderr)

	res = runOsapi(t, nil, put...)
	require.Equal(t, 1, res.exitCode, "plain put again: stdout: %s stderr: %s", res.stdout, res.stderr)
	assert.Contains(t, res.stderr, "409")

	res = runOsapi(t, nil, adminArgs("ism", "set", ismConflictID, "-d", "@"+ismFixture)...)
	require.Equal(t, 0, res.exitCode, "set on existing: stderr: %s", res.stderr)
}
