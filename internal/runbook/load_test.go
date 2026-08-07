package runbook

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
)

func TestLoadHappyPath(t *testing.T) {
	const doc = `
calls:
  health:
    path: _cluster/health
  bulk-index:
    path: _bulk
    method: POST
    retry: -1
    backoff: exponential
    backoff-initial: "500ms"
    backoff-max: "10s"
    backoff-jitter: 0.5
    max-body-buffer: "1MiB"
    abort-on: [404, 409]
    retry-when: '.errors'
    success-when: '.ok'
    stop-on-failure: true
  wait:
    path: _cluster/health
    verify-with: health
`
	rb, err := Load(strings.NewReader(doc))
	require.NoError(t, err)
	require.Len(t, rb.Calls, 3)

	health := rb.Calls[0]
	assert.Equal(t, "health", health.Name)
	assert.Equal(t, http.MethodGet, health.Method)
	assert.Equal(t, "_cluster/health", health.Path)
	assert.False(t, health.HasBody)
	assert.Equal(t, config.Defaults().Retry, health.Retry, "omitted retry fields must equal config.Defaults().Retry field-for-field")

	bulk := rb.Calls[1]
	assert.Equal(t, "bulk-index", bulk.Name)
	assert.Equal(t, http.MethodPost, bulk.Method)
	assert.Equal(t, "_bulk", bulk.Path)
	assert.Equal(t, -1, bulk.Retry.MaxRetries)
	assert.Equal(t, config.Exponential, bulk.Retry.Strategy)
	assert.Equal(t, 500*time.Millisecond, bulk.Retry.Initial)
	assert.Equal(t, int64(1<<20), bulk.Retry.MaxBodyBuffer)
	assert.Equal(t, 10*time.Second, bulk.Retry.Max)
	assert.Equal(t, 0.5, bulk.Retry.Jitter)
	assert.Equal(t, []int{404, 409}, bulk.Retry.AbortOn)
	require.NotNil(t, bulk.RetryWhen, "retry-when must compile into a predicate")
	require.NotNil(t, bulk.SuccessWhen, "success-when must compile into a predicate")
	assert.Equal(t, ".errors", bulk.Retry.RetryWhen)
	assert.Equal(t, ".ok", bulk.Retry.SuccessWhen)
	assert.True(t, bulk.StopOnFailure)

	wait := rb.Calls[2]
	assert.Equal(t, "health", wait.VerifyWith)
	assert.False(t, wait.StopOnFailure)

	assert.Nil(t, rb.Entries, "cross-call validation is a later section: Entries must stay unpopulated")
}

func TestLoadDependsOn(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "scalar",
			doc: `
calls:
  a:
    path: _cluster/health
  b:
    path: _cluster/health
    depends-on: a
`,
			want: []string{"a"},
		},
		{
			name: "sequence",
			doc: `
calls:
  a:
    path: _cluster/health
  b:
    path: _cluster/health
    depends-on: [a, c]
`,
			want: []string{"a", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rb, err := Load(strings.NewReader(tt.doc))
			require.NoError(t, err)
			require.Len(t, rb.Calls, 2)
			assert.Equal(t, tt.want, rb.Calls[1].DependsOn)
		})
	}
}

func TestLoadQueryAndHeaders(t *testing.T) {
	const doc = `
calls:
  search:
    path: _search
    query:
      pretty: "true"
      size: "5"
    headers:
      x-custom-header: value1
      content-type: application/json
`
	rb, err := Load(strings.NewReader(doc))
	require.NoError(t, err)
	require.Len(t, rb.Calls, 1)

	call := rb.Calls[0]
	assert.Equal(t, map[string]string{"pretty": "true", "size": "5"}, call.Query)
	assert.Equal(t, "value1", call.Headers.Get("X-Custom-Header"))
	assert.Equal(t, "application/json", call.Headers.Get("Content-Type"))
}

func TestLoadUnknownKeyError(t *testing.T) {
	const doc = `
calls:
  bad:
    path: _cluster/health
    succes-when: "true"
`
	_, err := Load(strings.NewReader(doc))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"bad"`)
	assert.Contains(t, err.Error(), "line 5", "must anchor to the offending key, not the call header")
	assert.Contains(t, err.Error(), "succes-when")
}

func TestLoadDuplicateCallNameError(t *testing.T) {
	const doc = `
calls:
  dup:
    path: _cluster/health
  dup:
    path: _search
`
	_, err := Load(strings.NewReader(doc))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"dup"`)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestLoadMissingPathError(t *testing.T) {
	const doc = `
calls:
  noPath:
    method: GET
`
	_, err := Load(strings.NewReader(doc))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"noPath"`)
	assert.Contains(t, err.Error(), "path")
}

func TestLoadBodyStdinRejected(t *testing.T) {
	const doc = `
calls:
  create:
    path: idx/_doc/1
    body: "@-"
`
	_, err := Load(strings.NewReader(doc))
	require.Error(t, err)
	assert.ErrorIs(t, err, osclient.ErrNoStdin)
	assert.Contains(t, err.Error(), `"create"`)
}

func TestLoadBodyFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "body.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"file":true}`), 0o600))

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "reads file contents", body: "@" + file},
		{name: "missing file wraps read error", body: "@" + filepath.Join(dir, "missing.json"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := "calls:\n  create:\n    path: idx/_doc/1\n    body: \"" + tt.body + "\"\n"
			rb, err := Load(strings.NewReader(doc))
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), `"create"`)
				return
			}
			require.NoError(t, err)
			require.Len(t, rb.Calls, 1)
			assert.True(t, rb.Calls[0].HasBody)
			assert.Equal(t, `{"file":true}`, string(rb.Calls[0].Body))
		})
	}
}

func TestLoadInvalidFieldsError(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "bad duration",
			doc: `
calls:
  bad:
    path: _cluster/health
    backoff-initial: "not-a-duration"
`,
		},
		{
			name: "bad size",
			doc: `
calls:
  bad:
    path: _cluster/health
    max-body-buffer: "not-a-size"
`,
		},
		{
			name: "bad backoff strategy",
			doc: `
calls:
  bad:
    path: _cluster/health
    backoff: fibonacci
`,
		},
		{
			name: "bad jq in retry-when",
			doc: `
calls:
  bad:
    path: _cluster/health
    retry-when: "("
`,
		},
		{
			name: "bad jq in success-when",
			doc: `
calls:
  bad:
    path: _cluster/health
    success-when: "("
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.doc))
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"bad"`)
		})
	}
}

func TestLoadTopLevelErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "unknown top-level key",
			doc: `
foo: bar
calls:
  a:
    path: _cluster/health
`,
		},
		{
			name: "calls not a mapping",
			doc: `
calls: not-a-mapping
`,
		},
		{
			name: "calls is a sequence",
			doc: `
calls:
  - path: _cluster/health
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.doc))
			require.Error(t, err)
		})
	}
}

// allowedCallKeys and callSpec's yaml tags enumerate the same key set in two
// places; this pins them together so adding a field to one without the other
// fails loudly instead of rejecting a valid key or silently dropping one.
func TestAllowedCallKeysMatchesCallSpec(t *testing.T) {
	specKeys := make(map[string]bool)
	for _, f := range reflect.VisibleFields(reflect.TypeOf(callSpec{})) {
		tag := f.Tag.Get("yaml")
		require.NotEmpty(t, tag, "callSpec field %s has no yaml tag", f.Name)
		specKeys[strings.Split(tag, ",")[0]] = true
	}
	assert.Equal(t, specKeys, allowedCallKeys)
}

func TestLoadDocumentShapeErrors(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name:    "empty file",
			doc:     "",
			wantErr: "runbook is empty",
		},
		{
			name:    "missing calls key",
			doc:     "{}\n",
			wantErr: `missing required key "calls"`,
		},
		{
			name: "second YAML document rejected",
			doc: `
calls:
  a:
    path: _cluster/health
---
calls:
  b:
    path: _cluster/health
`,
			wantErr: "single YAML document",
		},
		{
			name: "non-scalar call name",
			doc: `
calls:
  ? [a, b]
  : path: _cluster/health
`,
			wantErr: "call name must be a non-empty scalar",
		},
		{
			name: "empty call name",
			doc: `
calls:
  "":
    path: _cluster/health
`,
			wantErr: "call name must be a non-empty scalar",
		},
		{
			name: "alias call spec rejected",
			doc: `
calls:
  a: &spec
    path: _cluster/health
  b: *spec
`,
			wantErr: "aliases are not supported",
		},
		{
			name: "case-variant duplicate header",
			doc: `
calls:
  a:
    path: _cluster/health
    headers:
      content-type: one
      Content-Type: two
`,
			wantErr: "more than once with different casing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.doc))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
