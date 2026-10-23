package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

const wantPolicyPath = "/_plugins/_ism/policies/p"

// ismConn returns the flags that point a command at srv with credentials.
func ismConn(srv *httptest.Server) []string {
	return []string{"--endpoint", srv.URL, "-u", "admin", "--password", "secret"}
}

func ismArgs(srv *httptest.Server, args ...string) []string {
	return append(ismConn(srv), args...)
}

func getBody(policy string) string {
	return `{"_id":"p","_seq_no":1,"_primary_term":1,"policy":` + policy + `}`
}

func decodeObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m), string(raw))
	return m
}

func TestISMSetTemplate(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{getBody(`{"description":"old"}`), `{"ok":1}`})

	file := `{"policy":{"description":"keep <&> é","default_state":"a","states":[],` +
		`"ism_template":[{"index_patterns":["old"],"priority":1}]}}`
	stdout, _, err := run(t, nil, ismArgs(srv, "ism", "set", "p", "-d", file,
		"--target", "a-*", "--target", "-b-*", "--priority", "10")...)
	require.NoError(t, err)
	assert.Equal(t, `{"ok":1}`, stdout)

	require.Equal(t, 2, rec.len())
	put := rec.at(1)
	assert.Equal(t, http.MethodPut, put.method)
	assert.Equal(t, wantPolicyPath, put.path)

	policy := decodeObject(t, decodeObject(t, put.body)["policy"])
	assert.Equal(t, `"keep <&> é"`, string(policy["description"]))
	assert.JSONEq(t, `[{"index_patterns":["a-*","-b-*"],"priority":10}]`, string(policy["ism_template"]))
}

func TestISMSetStripsServerFields(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK},
		[]string{getBody(`{"description":"old"}`), `{}`})

	in := `{"_id":"p","_seq_no":7,"_primary_term":2,"policy":{` +
		`"policy_id":"p","last_updated_time":5,"user":{"name":"u"},"schema_version":19,` +
		`"description":"d","default_state":"a","states":[{"name":"a","actions":[{"retry":{"count":3}}]}],` +
		`"ism_template":[{"index_patterns":["x*"],"priority":1,"last_updated_time":6}]}}`
	_, _, err := run(t, strings.NewReader(in), ismArgs(srv, "ism", "set", "p", "-d", "@-")...)
	require.NoError(t, err)

	require.Equal(t, 2, rec.len())
	body := decodeObject(t, rec.at(1).body)
	require.Len(t, body, 1)
	policy := decodeObject(t, body["policy"])
	for _, k := range []string{"policy_id", "last_updated_time", "user", "schema_version"} {
		assert.NotContains(t, policy, k)
	}
	assert.JSONEq(t, `[{"name":"a","actions":[{"retry":{"count":3}}]}]`, string(policy["states"]))
	assert.Equal(t, `[{"index_patterns":["x*"],"priority":1}]`, string(policy["ism_template"]))
}

func TestISMSetWrapsTemplateObject(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{getBody(`{"description":"old"}`), `{}`})

	in := `{"policy":{"ism_template":{"index_patterns":["x*"],"priority":1,"last_updated_time":6}}}`
	_, _, err := run(t, nil, ismArgs(srv, "ism", "set", "p", "-d", in)...)
	require.NoError(t, err)

	require.Equal(t, 2, rec.len())
	policy := decodeObject(t, decodeObject(t, rec.at(1).body)["policy"])
	assert.Equal(t, `[{"index_patterns":["x*"],"priority":1}]`, string(policy["ism_template"]))
}

func TestISMSetConflictRetried(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec,
		[]int{http.StatusOK, http.StatusConflict, http.StatusOK, http.StatusOK},
		[]string{
			`{"_seq_no":1,"_primary_term":1,"policy":{"description":"a"}}`, "conflict",
			`{"_seq_no":2,"_primary_term":1,"policy":{"description":"a"}}`, `{"final":true}`,
		})

	stdout, _, err := run(t, nil, ismArgs(srv, "ism", "set", "p", "-d", `{"policy":{"description":"b"}}`,
		"--retry", "1", "--retry-on", "409", "--backoff-initial", "1ms")...)
	require.NoError(t, err)
	assert.Equal(t, `{"final":true}`, stdout)

	require.Equal(t, 4, rec.len())
	assert.Equal(t, "1", rec.at(1).query.Get("if_seq_no"))
	assert.Equal(t, "2", rec.at(3).query.Get("if_seq_no"))
}

func TestISMSetConflictExhausted(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK, http.StatusConflict},
		[]string{getBody(`{"description":"a"}`), "conflict"})

	stdout, _, err := run(t, nil, ismArgs(srv, "ism", "set", "p", "-d", `{"policy":{"description":"b"}}`,
		"--retry", "0")...)
	require.ErrorIs(t, err, retry.ErrStatusRejected)
	assert.Equal(t, "conflict", stdout)
}

func TestISMSetMalformedGetRetried(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK},
		[]string{`{"_seq_no":"x"}`, getBody(`{"description":"a"}`), `{"ok":1}`})

	stdout, stderr, err := run(t, nil, ismArgs(srv, "ism", "set", "p", "-d", `{"policy":{"description":"b"}}`,
		"--retry", "1", "--backoff-initial", "1ms", "-v")...)
	require.NoError(t, err)
	assert.Equal(t, `{"ok":1}`, stdout)
	assert.Contains(t, stderr, "attempt 1")
	assert.Contains(t, stderr, "_seq_no")

	require.Equal(t, 3, rec.len())
	assert.Equal(t, http.MethodGet, rec.at(1).method)
	assert.Equal(t, http.MethodPut, rec.at(2).method)
}

const storedPolicy = `{"policy_id":"p","last_updated_time":5,"schema_version":1,"description":"d",` +
	`"default_state":"a","states":[],"ism_template":[{"index_patterns":["x*"],"priority":1,"last_updated_time":5}]}`

func fileWithDescription(desc string) string {
	return `{"policy":{"description":"` + desc + `","default_state":"a","states":[],` +
		`"ism_template":{"index_patterns":["x*"],"priority":1}}}`
}

func TestISMSetUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		stored   string
		file     string
		extra    []string
		retries  bool
		wantReqs int
	}{
		{name: "same policy sends no PUT", stored: storedPolicy, file: fileWithDescription("d"), wantReqs: 1},
		{name: "one character differs sends PUT", stored: storedPolicy, file: fileWithDescription("e"), wantReqs: 2},
		{
			name: "--target equal to the stored template is unchanged", stored: storedPolicy,
			file:  `{"policy":{"description":"d","default_state":"a","states":[]}}`,
			extra: []string{"--target", "x*", "--priority", "1"}, wantReqs: 1,
		},
		{
			name:   "integers past 2^53 compare exactly",
			stored: `{"description":"d","priority":9007199254740993}`,
			file:   `{"policy":{"description":"d","priority":9007199254740992}}`, wantReqs: 2,
		},
		{
			name: "unchanged is reported once across attempts", stored: storedPolicy, file: fileWithDescription("d"),
			extra: []string{"--retry", "1", "--backoff-initial", "1ms", "--retry-when", "true"}, wantReqs: 2, retries: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec capture
			stored := getBody(tt.stored)
			bodies := []string{stored, `{"put":true}`}
			if tt.retries {
				bodies = []string{stored, stored} // every attempt lands on the unchanged path
			}
			srv := newBodyServer(t, &rec, []int{http.StatusOK}, bodies)

			args := append(ismArgs(srv, "ism", "set", "p", "-d", tt.file), tt.extra...)
			stdout, stderr, err := run(t, nil, args...)
			require.Equal(t, tt.wantReqs, rec.len())
			if tt.retries {
				require.Error(t, err)
				assert.Equal(t, 1, strings.Count(stderr, "policy p unchanged"), stderr)
				return
			}
			require.NoError(t, err)
			if tt.wantReqs == 1 {
				assert.Equal(t, stored, stdout)
				assert.Contains(t, stderr, "policy p unchanged")
				return
			}
			assert.Equal(t, `{"put":true}`, stdout)
			assert.NotContains(t, stderr, "unchanged")
		})
	}
}

func TestISMSetForce(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{getBody(storedPolicy), `{"put":true}`})

	stdout, stderr, err := run(t, nil, ismArgs(srv, "ism", "set", "p", "-d", fileWithDescription("d"), "--force")...)
	require.NoError(t, err)
	assert.Equal(t, `{"put":true}`, stdout)
	assert.NotContains(t, stderr, "unchanged")
	require.Equal(t, 2, rec.len())
	assert.Equal(t, http.MethodPut, rec.at(1).method)
}

func TestISMSetValidation(t *testing.T) {
	const ok = `{"policy":{"description":"d"}}`
	tests := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{name: "priority without target", args: []string{"set", "p", "-d", ok, "--priority", "5"}, wantSub: "priority"},
		{name: "missing body", args: []string{"set", "p"}, wantSub: "body"},
		{name: "no policy object", args: []string{"set", "p", "-d", `{"x":1}`},
			wantSub: `policy body: top-level "policy" object is required`},
		{name: "policy not object", args: []string{"set", "p", "-d", `{"policy":"x"}`},
			wantSub: `"policy" must be an object`},
		{name: "policy null", args: []string{"set", "p", "-d", `{"policy":null}`},
			wantSub: `"policy" must be an object`},
		{name: "ism_template string", args: []string{"set", "p", "-d", `{"policy":{"ism_template":"x"}}`},
			wantSub: "ism_template"},
		{name: "ism_template null", args: []string{"set", "p", "-d", `{"policy":{"ism_template":null}}`},
			wantSub: "ism_template"},
		{name: "ism_template entry not object", args: []string{"set", "p", "-d", `{"policy":{"ism_template":[1]}}`},
			wantSub: "ism_template"},
		{name: "top level not object", args: []string{"set", "p", "-d", `[]`}, wantSub: "policy body"},
		{name: "trailing data", args: []string{"set", "p", "-d", `{"policy":{}} x`}, wantSub: "after the JSON value"},
		{name: "id with slash", args: []string{"set", "a/b", "-d", ok}, wantSub: "a/b"},
		{name: "empty id", args: []string{"set", "", "-d", ok}, wantSub: "must be a name"},
		{name: "dot id", args: []string{"set", "..", "-d", ok}, wantSub: "must be a name"},
		{name: "reapply target with slash", args: []string{"reapply", "p", "--target", "a/b"}, wantSub: "a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec capture
			srv := newServer(t, &rec, []int{http.StatusOK})

			_, _, err := run(t, nil, ismArgs(srv, append([]string{"ism"}, tt.args...)...)...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantSub)
			assert.Equal(t, 0, rec.len())
		})
	}
}

func TestISMFlagPosition(t *testing.T) {
	policy := `{"policy":{"description":"d"}}`
	tests := []struct {
		name string
		args func(srv *httptest.Server, envFile string) []string
	}{
		{name: "before ism", args: func(srv *httptest.Server, _ string) []string {
			return append(ismConn(srv), "-k", "ism", "set", "p", "-d", policy)
		}},
		{name: "between ism and verb", args: func(srv *httptest.Server, _ string) []string {
			return append(append([]string{"ism"}, ismConn(srv)...), "set", "p", "-d", policy)
		}},
		{name: "after verb", args: func(srv *httptest.Server, _ string) []string {
			return append([]string{"ism", "set", "p", "-d", policy}, ismConn(srv)...)
		}},
		{name: "env file", args: func(_ *httptest.Server, envFile string) []string {
			return []string{"ism", "--env-file", envFile, "set", "p", "-d", policy}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec capture
			srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{getBody(`{"description":"a"}`), `{}`})
			envFile := writeEnvFile(t, "OPENSEARCH_URL="+srv.URL+"\nOPENSEARCH_USERNAME=admin\nOPENSEARCH_PASSWORD=secret\n")

			_, _, err := run(t, nil, tt.args(srv, envFile)...)
			require.NoError(t, err)
			require.Equal(t, 2, rec.len())
			for i := range 2 {
				got := rec.at(i)
				assert.True(t, got.authOK)
				assert.Equal(t, "admin", got.user)
				assert.Equal(t, "secret", got.pass)
			}
		})
	}
}

func TestISMRejectsRootOnlyFlags(t *testing.T) {
	_, _, err := run(t, nil, "ism", "set", "p", "-d", "x", "--path", "/x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag")
}

func TestISMArgCounts(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "set none", args: []string{"set", "-d", "{}"}},
		{name: "set two", args: []string{"set", "a", "b", "-d", "{}"}},
		{name: "reapply none", args: []string{"reapply"}},
		{name: "reapply two", args: []string{"reapply", "a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec capture
			srv := newServer(t, &rec, []int{http.StatusOK})
			_, _, err := run(t, nil, ismArgs(srv, append([]string{"ism"}, tt.args...)...)...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "arg")
			assert.Equal(t, 0, rec.len())
		})
	}
}

const explainAll = `{"i1":{"policy_id":"p"},"i2":{"policy_id":"q"},"i0":{"policy_id":"p"},"total_managed_indices":3}`

const changeOK = `{"updated_indices":1,"failures":false,"failed_indices":[]}`

func TestISMReapplyAll(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{explainAll, changeOK, changeOK})

	stdout, _, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p")...)
	require.NoError(t, err)
	assert.JSONEq(t, `{"updated_indices":2,"failures":false,"failed_indices":[]}`, stdout)

	require.Equal(t, 3, rec.len())
	get := rec.at(0)
	assert.Equal(t, http.MethodGet, get.method)
	assert.Equal(t, "/_plugins/_ism/explain", get.path)
	assert.Equal(t, "10000", get.query.Get("size"))
	for i, idx := range []string{"i0", "i1"} {
		post := rec.at(i + 1)
		assert.Equal(t, http.MethodPost, post.method)
		assert.Equal(t, "/_plugins/_ism/change_policy/"+idx, post.path)
		assert.JSONEq(t, `{"policy_id":"p"}`, string(post.body))
	}
}

func TestISMReapplyTarget(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{explainAll, changeOK, changeOK})

	_, _, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p", "--target", "x-*,-x-audit-*")...)
	require.NoError(t, err)
	assert.Equal(t, "/_plugins/_ism/explain/x-*,-x-audit-*", rec.at(0).path)
	assert.Equal(t, "10000", rec.at(0).query.Get("size"))
	require.Equal(t, 3, rec.len())
	assert.Equal(t, "/_plugins/_ism/change_policy/i0", rec.at(1).path)
}

func TestISMReapplyMerge(t *testing.T) {
	f := func(s string) json.RawMessage { return json.RawMessage(s) }
	tests := []struct {
		name string
		in   []changePolicyResult
		want changePolicyResult
	}{
		{
			name: "sums updated indices",
			in:   []changePolicyResult{{UpdatedIndices: 1}, {UpdatedIndices: 3}},
			want: changePolicyResult{UpdatedIndices: 4, FailedIndices: []json.RawMessage{}},
		},
		{
			name: "ors failures and concatenates failed indices in order",
			in: []changePolicyResult{
				{UpdatedIndices: 1, Failures: true, FailedIndices: []json.RawMessage{f(`{"index_name":"a"}`)}},
				{UpdatedIndices: 1},
				{Failures: true, FailedIndices: []json.RawMessage{f(`{"index_name":"c"}`), f(`{"index_name":"d"}`)}},
			},
			want: changePolicyResult{
				UpdatedIndices: 2,
				Failures:       true,
				FailedIndices:  []json.RawMessage{f(`{"index_name":"a"}`), f(`{"index_name":"c"}`), f(`{"index_name":"d"}`)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeChangePolicy(tt.in)
			assert.Equal(t, tt.want, got)
			out, err := marshalRaw(got)
			require.NoError(t, err)
			assert.NotContains(t, string(out), "null")
		})
	}
}

func TestISMReapplyPartialFailure(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK, http.StatusInternalServerError},
		[]string{explainAll, "boom"})

	stdout, _, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p")...)
	require.ErrorIs(t, err, retry.ErrStatusRejected)
	assert.Contains(t, err.Error(), "change_policy i0")
	assert.Equal(t, "boom", stdout)
	assert.Equal(t, 2, rec.len(), "the loop must stop at the first failure")
}

func TestISMReapplyNonJSONBody(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{explainAll, "<html>"})

	stdout, _, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding change_policy response for i0")
	assert.Equal(t, "<html>", stdout)
	assert.Equal(t, 2, rec.len())
}

func TestISMReapplyPredicateError(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{explainAll, changeOK})

	_, stderr, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p", "--success-when", `error("x")`)...)
	require.ErrorIs(t, err, retry.ErrStatusRejected)
	assert.Contains(t, stderr, "treating it as not matched")
}

func TestISMReapplyFailuresMerged(t *testing.T) {
	failed := `{"updated_indices":0,"failures":true,"failed_indices":[{"index_name":"i1","reason":"nope"}]}`
	want := `{"updated_indices":1,"failures":true,"failed_indices":[{"index_name":"i1","reason":"nope"}]}`

	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{explainAll, changeOK, failed})

	stdout, _, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p")...)
	require.NoError(t, err)
	assert.JSONEq(t, want, stdout)
}

func TestISMReapplyNoneManaged(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{`{"i2":{"policy_id":"q"},"total_managed_indices":1}`})

	stdout, stderr, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p")...)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "no indices are managed by p")
	assert.Equal(t, 1, rec.len())
}

func TestISMReapplyExplainFailure(t *testing.T) {
	var rec capture
	srv := newBodyServer(t, &rec, []int{http.StatusInternalServerError}, []string{"boom"})

	stdout, _, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p")...)
	require.ErrorIs(t, err, retry.ErrStatusRejected)
	assert.Equal(t, "boom", stdout)
	assert.Equal(t, 1, rec.len())
}

func TestISMRetryFlags(t *testing.T) {
	t.Run("set retries a 503 GET", func(t *testing.T) {
		var rec capture
		srv := newBodyServer(t, &rec, []int{http.StatusServiceUnavailable, http.StatusOK, http.StatusOK},
			[]string{"busy", getBody(`{"description":"a"}`), `{"ok":1}`})

		_, _, err := run(t, nil, ismArgs(srv, "ism", "set", "p", "-d", `{"policy":{"description":"b"}}`,
			"--retry", "1", "--backoff-initial", "1ms")...)
		require.NoError(t, err)
		require.Equal(t, 3, rec.len())
		assert.Equal(t, http.MethodPut, rec.at(2).method)
	})

	t.Run("reapply success-when judges the change_policy body", func(t *testing.T) {
		var rec capture
		srv := newBodyServer(t, &rec, []int{http.StatusOK}, []string{explainAll, changeOK,
			`{"updated_indices":0,"failures":true,"failed_indices":[{"index_name":"i1"}]}`})

		stdout, _, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p", "--success-when", ".failures == false")...)
		require.ErrorIs(t, err, retry.ErrStatusRejected)
		assert.JSONEq(t, `{"updated_indices":1,"failures":true,"failed_indices":[{"index_name":"i1"}]}`, stdout)
		assert.Equal(t, 3, rec.len())
	})

	t.Run("abort-on with retry-on is rejected before any request", func(t *testing.T) {
		var rec capture
		srv := newServer(t, &rec, []int{http.StatusOK})

		_, _, err := run(t, nil, ismArgs(srv, "ism", "reapply", "p", "--abort-on", "400", "--retry-on", "409")...)
		require.Error(t, err)
		assert.Equal(t, 0, rec.len())
	})
}
