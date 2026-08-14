package runbook

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
)

func TestLoadHappyPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index-settings.json"), []byte(`{"settings":{}}`), 0o600))

	src := `
calls:
  - name: drop_stale_index
    method: DELETE
    path: /stale-index
    success-when: '.acknowledged == true or .status == 404'

  - name: create_index
    method: PUT
    path: /my-index
    body: '@index-settings.json'
    success-when: '.acknowledged'
    retry: -1
    backoff: exponential
    backoff-initial: '500ms'
    max-body-buffer: '1MiB'

  - name: index_doc
    method: PUT
    path: /my-index/_doc/1
    body: '{"field":"initial"}'
`
	rb, err := Load(strings.NewReader(src), dir)
	require.NoError(t, err)
	require.Len(t, rb.Calls, 3)

	names := make([]string, len(rb.Calls))
	for i, c := range rb.Calls {
		names[i] = c.Name
	}
	assert.Equal(t, []string{"drop_stale_index", "create_index", "index_doc"}, names)

	assert.Equal(t, "DELETE", rb.Calls[0].Method)
	assert.Equal(t, "/stale-index", rb.Calls[0].Path)
	assert.NotNil(t, rb.Calls[0].SuccessWhen, "success-when is compiled at load")
	assert.Nil(t, rb.Calls[0].RetryWhen, "no retry-when means no predicate")
	assert.Equal(t, ".acknowledged == true or .status == 404", rb.Calls[0].Retry.SuccessWhen)

	// omitted retry fields equal config.Defaults().Retry field-for-field.
	assert.Equal(t, config.Defaults().Retry, rb.Calls[2].Retry)

	// explicit retry/backoff/size fields parse via config helpers.
	got := rb.Calls[1].Retry
	assert.Equal(t, -1, got.MaxRetries)
	assert.Equal(t, config.Exponential, got.Strategy)
	assert.Equal(t, 500*time.Millisecond, got.Initial)
	assert.Equal(t, int64(1<<20), got.MaxBodyBuffer)
	assert.True(t, rb.Calls[1].HasBody)
	assert.Equal(t, []byte(`{"settings":{}}`), rb.Calls[1].Body)

	assert.True(t, rb.Calls[2].HasBody)
	assert.Equal(t, []byte(`{"field":"initial"}`), rb.Calls[2].Body)
}

func TestLoadQueryAndHeaders(t *testing.T) {
	src := `
calls:
  - name: update_doc
    method: PUT
    path: /my-index/_doc/1
    query:
      if_seq_no: '5'
      if_primary_term: '1'
    headers:
      content-type: application/json
      x-custom: value
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)
	require.Len(t, rb.Calls, 1)

	call := rb.Calls[0]
	assert.Equal(t, map[string]string{"if_seq_no": "5", "if_primary_term": "1"}, call.Query)
	assert.Equal(t, "application/json", call.Headers.Get("Content-Type"))
	assert.Equal(t, "value", call.Headers.Get("X-Custom"))
}

func TestLoadContinueOnFailure(t *testing.T) {
	src := `
calls:
  - name: warm_caches
    method: POST
    path: /my-index/_forcemerge
    continue-on-failure: true
  - name: other_call
    path: /other
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)
	require.Len(t, rb.Calls, 2)

	assert.True(t, rb.Calls[0].ContinueOnFailure)
	assert.False(t, rb.Calls[1].ContinueOnFailure)
}

func TestLoadCallErrors(t *testing.T) {
	tests := []struct {
		name       string
		src        string
		wantErrMsg []string
	}{
		{
			name: "unknown per-call key names the call and line",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    succes-when: '._seq_no != null'
`,
			wantErrMsg: []string{"read_doc", "line 3", `unknown key "succes-when"`},
		},
		{
			name: "duplicate call name names both lines",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
  - name: read_doc
    path: /my-index/_doc/2
`,
			wantErrMsg: []string{"read_doc", "line 3", "line 5", "duplicate"},
		},
		{
			name: "missing path names the call",
			src: `
calls:
  - name: read_doc
`,
			wantErrMsg: []string{"read_doc", "path"},
		},
		{
			name: "missing name names the line only",
			src: `
calls:
  - path: /my-index/_doc/1
`,
			wantErrMsg: []string{"line 3", "name"},
		},
		{
			name: "call item that is a sequence is not mistaken for a mapping",
			src: `
calls:
  - [name, read_doc]
`,
			wantErrMsg: []string{"line 3", "must be a mapping", "sequence"},
		},
		{
			name: "call item that is a scalar",
			src: `
calls:
  - read_doc
`,
			wantErrMsg: []string{"line 3", "must be a mapping", "scalar"},
		},
		{
			name: "bare @ body is rejected rather than reading the base directory",
			src: `
calls:
  - name: read_doc
    path: /x
    body: '@'
`,
			wantErrMsg: []string{"read_doc", `"@" needs a file path`},
		},
		{
			name: "invalid method token names the call and the method",
			src: `
calls:
  - name: read_doc
    path: /x
    method: 'GET EXTRA'
`,
			wantErrMsg: []string{"read_doc", "method", "GET EXTRA"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.src), "")
			require.Error(t, err)
			for _, want := range tt.wantErrMsg {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

// Duplicate detection must not be gated behind body reads or jq compilation,
// or which error a duplicated call reports depends on the filesystem.
func TestLoadDuplicateNameBeatsPerCallErrors(t *testing.T) {
	src := `
calls:
  - name: read_doc
    path: /a
  - name: read_doc
    path: /b
    body: '@definitely-missing.json'
`
	_, err := Load(strings.NewReader(src), t.TempDir())
	require.Error(t, err)
	assert.ErrorContains(t, err, "duplicate")
	assert.NotContains(t, err.Error(), "definitely-missing.json")
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	src := `
calls:
  - name: first
    path: /a
---
calls:
  - name: second
    path: /b
`
	_, err := Load(strings.NewReader(src), "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "single YAML document")
}

func TestLoadEmptyInput(t *testing.T) {
	for _, src := range []string{"", "# just a comment\n", "   \n"} {
		_, err := Load(strings.NewReader(src), "")
		require.Error(t, err)
		assert.ErrorContains(t, err, "calls:", "an empty runbook must not surface a bare EOF")
		assert.NotErrorIs(t, err, io.EOF)
	}
}

func TestLoadStdinBodyRejected(t *testing.T) {
	src := `
calls:
  - name: index_doc
    path: /my-index/_doc/1
    body: '@-'
`
	_, err := Load(strings.NewReader(src), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, osclient.ErrNoStdin)
	assert.ErrorContains(t, err, "index_doc")
}

func TestLoadBodyFileResolution(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	require.NoError(t, os.Mkdir(runDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "payload.json"), []byte(`{"in":"rundir"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "shared.json"), []byte(`{"in":"root"}`), 0o600))

	absFile := filepath.Join(t.TempDir(), "abs.json")
	require.NoError(t, os.WriteFile(absFile, []byte(`{"in":"abs"}`), 0o600))

	tests := []struct {
		name     string
		body     string
		wantBody string
		wantErr  string
	}{
		{name: "relative resolves against baseDir", body: "@payload.json", wantBody: `{"in":"rundir"}`},
		{name: "sibling directory via ../", body: "@../shared.json", wantBody: `{"in":"root"}`},
		{name: "absolute path used as given", body: "@" + absFile, wantBody: `{"in":"abs"}`},
		{name: "missing file wraps read error naming resolved path", body: "@missing.json", wantErr: filepath.Join(runDir, "missing.json")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "calls:\n  - name: c\n    path: /x\n    body: '" + tt.body + "'\n"
			rb, err := Load(strings.NewReader(src), runDir)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, rb.Calls, 1)
			assert.Equal(t, []byte(tt.wantBody), rb.Calls[0].Body)
		})
	}
}

func TestLoadInvalidFieldValues(t *testing.T) {
	tests := []struct {
		name    string
		callKey string
	}{
		{name: "bad duration", callKey: "backoff-initial: 'notaduration'"},
		{name: "bad size", callKey: "max-body-buffer: 'notasize'"},
		{name: "bad backoff strategy", callKey: "backoff: 'not-a-strategy'"},
		{name: "bad retry-when jq", callKey: "retry-when: '.foo['"},
		{name: "bad success-when jq", callKey: "success-when: '.foo['"},
		{name: "bad method", callKey: "method: 'GET EXTRA'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "calls:\n  - name: read_doc\n    path: /x\n    " + tt.callKey + "\n"
			_, err := Load(strings.NewReader(src), "")
			require.Error(t, err)
			assert.ErrorContains(t, err, "read_doc")
			assert.ErrorContains(t, err, "line 2")
		})
	}
}

// A method that is a valid HTTP token, or an omitted method, must keep
// loading: validation must accept exactly what osclient.BuildRequest ->
// http.NewRequest will accept downstream, not a stricter set.
func TestLoadValidMethodTokens(t *testing.T) {
	tests := []struct {
		name   string
		method string
	}{
		{name: "lowercase standard verb", method: "get"},
		{name: "custom RFC 7230 token", method: "PURGE"},
		{name: "omitted method defaults to GET downstream", method: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			methodLine := ""
			if tt.method != "" {
				methodLine = "    method: '" + tt.method + "'\n"
			}
			src := "calls:\n  - name: c\n    path: /x\n" + methodLine
			rb, err := Load(strings.NewReader(src), "")
			require.NoError(t, err)
			require.Len(t, rb.Calls, 1)
			assert.Equal(t, tt.method, rb.Calls[0].Method)
		})
	}
}

func TestLoadTopLevelSchemaErrors(t *testing.T) {
	tests := []struct {
		name       string
		src        string
		wantErrMsg []string
	}{
		{
			name: "unknown top-level key",
			src: `
credentials:
  user: admin
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{"credentials"},
		},
		{
			name: "calls as a mapping instead of a sequence",
			src: `
calls:
  name: c
  path: /x
`,
			wantErrMsg: []string{"calls", "must be a sequence", "mapping"},
		},
		{
			name: "calls empty",
			src: `
calls: []
`,
			wantErrMsg: []string{"calls", "must not be empty"},
		},
		{
			name: "calls present but null",
			src: `
calls:
`,
			wantErrMsg: []string{"calls", "must not be empty"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.src), "")
			require.Error(t, err)
			for _, want := range tt.wantErrMsg {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestLoadDefaultsLayering(t *testing.T) {
	src := `
defaults:
  retry: 3
  abort-on: [400, 401, 403]

calls:
  - name: uses_defaults
    path: /a
  - name: overrides_retry
    path: /b
    retry: 9
  - name: replaces_abort_on
    path: /c
    abort-on: [500]
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)
	require.Len(t, rb.Calls, 3)

	assert.Equal(t, 3, rb.Calls[0].Retry.MaxRetries)
	assert.Equal(t, []int{400, 401, 403}, rb.Calls[0].Retry.AbortOn)

	assert.Equal(t, 9, rb.Calls[1].Retry.MaxRetries)
	assert.Equal(t, []int{400, 401, 403}, rb.Calls[1].Retry.AbortOn)

	assert.Equal(t, 3, rb.Calls[2].Retry.MaxRetries)
	assert.Equal(t, []int{500}, rb.Calls[2].Retry.AbortOn, "abort-on replaces the defaults list rather than appending")

	// Calls that inherit abort-on must not share one backing array.
	rb.Calls[0].Retry.AbortOn[0] = 999
	assert.Equal(t, 400, rb.Calls[1].Retry.AbortOn[0], "inherited abort-on is cloned per call")
}

// Defaults must layer for every key kind, not just the two scalars the happy
// path covers, and a call's own query/headers must not leak into later calls —
// yaml.v3 decodes a mapping into an existing non-nil map in place.
func TestLoadDefaultsLayeringAcrossKeyKinds(t *testing.T) {
	src := `
defaults:
  method: POST
  backoff: exponential
  backoff-initial: '250ms'
  continue-on-failure: true
  success-when: '.ok'
  query:
    pretty: 'true'
  headers:
    x-shared: shared

calls:
  - name: inherits_everything
    path: /a
  - name: overrides_some
    path: /b
    method: PUT
    continue-on-failure: false
    query:
      size: '5'
    headers:
      x-own: own
  - name: inherits_again
    path: /c
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)
	require.Len(t, rb.Calls, 3)

	assert.Equal(t, "POST", rb.Calls[0].Method)
	assert.Equal(t, config.Exponential, rb.Calls[0].Retry.Strategy)
	assert.Equal(t, 250*time.Millisecond, rb.Calls[0].Retry.Initial)
	assert.True(t, rb.Calls[0].ContinueOnFailure)
	assert.NotNil(t, rb.Calls[0].SuccessWhen, "an inherited success-when is compiled too")
	assert.Equal(t, map[string]string{"pretty": "true"}, rb.Calls[0].Query)
	assert.Equal(t, "shared", rb.Calls[0].Headers.Get("X-Shared"))

	assert.Equal(t, "PUT", rb.Calls[1].Method)
	assert.False(t, rb.Calls[1].ContinueOnFailure)
	assert.Equal(t, map[string]string{"pretty": "true", "size": "5"}, rb.Calls[1].Query,
		"a call's query merges onto the inherited one")

	assert.Equal(t, map[string]string{"pretty": "true"}, rb.Calls[2].Query,
		"the previous call's query must not leak forward")
	assert.Empty(t, rb.Calls[2].Headers.Get("X-Own"),
		"the previous call's headers must not leak forward")
}

// A call's header must win over a same-named default header regardless of
// letter-case spelling on either side. Both spellings canonicalize to the
// same textproto key, and the old decode-in-place merge kept both as distinct
// map[string]string entries — buildHeaders then Set both, so the survivor
// depended on map iteration order. Looped: a single load passes ~88% of the
// time under the old bug, so one iteration would not reliably pin it.
func TestLoadCallHeaderOverridesDefaultRegardlessOfCase(t *testing.T) {
	src := `
defaults:
  headers:
    content-type: application/json

calls:
  - name: bulk
    path: /_bulk
    headers:
      Content-Type: application/x-ndjson
`
	for i := 0; i < 200; i++ {
		rb, err := Load(strings.NewReader(src), "")
		require.NoError(t, err)
		require.Len(t, rb.Calls, 1)
		require.Equal(t, "application/x-ndjson", rb.Calls[0].Headers.Get("Content-Type"),
			"iteration %d: the call's own header must beat the inherited default", i)
	}
}

// Headers keep defaults separate from the call's own (mergeHeaders), so
// inherited values get their own reference-check pass: an unresolvable ref in
// a defaults header fails the load unless the call overrides that header —
// by canonical name, any case spelling — in which case the default never
// ships and must not be checked.
func TestLoadInheritedHeaderRefs(t *testing.T) {
	src := `
defaults:
  headers:
    x-seq: '${seq}'
calls:
  - name: first
    path: /a
`
	_, err := Load(strings.NewReader(src), "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "inherited from defaults")

	overridden := strings.Replace(src, "path: /a\n", "path: /a\n    headers:\n      X-Seq: literal\n", 1)
	_, err = Load(strings.NewReader(overridden), "")
	require.NoError(t, err, "an overridden default header must not be ref-checked")
}

// capture: parses in document order; a call without it has an
// empty slice.
func TestLoadCaptureParsesInDocumentOrder(t *testing.T) {
	src := `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      seq: '._seq_no'
      term: '._primary_term'
  - name: other_call
    path: /other
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)
	require.Len(t, rb.Calls, 2)

	require.Len(t, rb.Calls[0].Capture, 2)
	assert.Equal(t, "seq", rb.Calls[0].Capture[0].Name)
	assert.Equal(t, "term", rb.Calls[0].Capture[1].Name)
	assert.Empty(t, rb.Calls[1].Capture)
}

func TestLoadCaptureErrors(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		wantContains []string
	}{
		{
			name: "bad jq in a capture expression names the call and the capture",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      seq: '.foo['
`,
			wantContains: []string{"read_doc", "seq"},
		},
		{
			name: "duplicate capture name across two calls names both calls",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      seq: '._seq_no'
  - name: read_doc_again
    path: /my-index/_doc/2
    capture:
      seq: '._seq_no'
`,
			wantContains: []string{"read_doc", "read_doc_again", "seq"},
		},
		{
			name: "duplicate capture name within a single call names the call, not itself twice",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      seq: '._seq_no'
      seq: '._primary_term'
`,
			wantContains: []string{"declared twice", "read_doc", "seq"},
		},
		{
			name: "unknown ${name} reference names the referencing call and the name",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
  - name: update_doc
    path: /my-index/_doc/${seq}
`,
			wantContains: []string{"update_doc", "seq"},
		},
		{
			name: "forward reference (capture declared by a later call)",
			src: `
calls:
  - name: update_doc
    path: /my-index/_doc/${seq}
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      seq: '._seq_no'
`,
			wantContains: []string{"update_doc", "seq"},
		},
		{
			name: "reference to a capture declared by a continue-on-failure: true call",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    continue-on-failure: true
    capture:
      seq: '._seq_no'
  - name: update_doc
    path: /my-index/_doc/${seq}
`,
			wantContains: []string{"update_doc", "read_doc", "seq", "continue-on-failure"},
		},
		{
			name: "unterminated ${ in a body is a load error, not a literal shipped to the server",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    body: '{"a":"x", "b":${seq'
`,
			wantContains: []string{"read_doc", "unterminated"},
		},
		{
			name: "${ in a query key is rejected rather than shipped or substituted",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    query:
      '${nope}': v
`,
			wantContains: []string{"read_doc", "query", "${nope}", "references are not supported in query/header keys"},
		},
		{
			name: "${ in a header key is rejected rather than shipped or substituted",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    headers:
      '${nope}': v
`,
			wantContains: []string{"read_doc", "header", "${nope}", "references are not supported in query/header keys"},
		},
		{
			name: "empty capture name",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      '': '._seq_no'
`,
			wantContains: []string{"read_doc", "name must match"},
		},
		{
			name: "capture name with a space",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      'has space': '._seq_no'
`,
			wantContains: []string{"read_doc", "has space", "name must match"},
		},
		{
			name: "capture name with a closing brace is permanently unreferenceable",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      'a}b': '._seq_no'
`,
			wantContains: []string{"read_doc", "a}b", "name must match"},
		},
		{
			name: "capture name that looks like a reference",
			src: `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      '${x}': '._seq_no'
`,
			wantContains: []string{"read_doc", "${x}", "name must match"},
		},
		{
			name: "an unresolvable reference inherited from defaults: says so",
			src: `
defaults:
  query:
    v: '${seq}'
calls:
  - name: first
    path: /a
`,
			wantContains: []string{"first", "seq", "inherited from defaults"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.src), "")
			require.Error(t, err)
			for _, want := range tt.wantContains {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

// ${name} in path, query value, header value and body all
// validate; $${literal} is accepted and does not count as a reference.
func TestLoadReferenceInEveryField(t *testing.T) {
	src := `
calls:
  - name: read_doc
    path: /my-index/_doc/1
    capture:
      seq: '._seq_no'
  - name: update_doc
    path: /my-index/_doc/${seq}
    query:
      version: '${seq}'
    headers:
      x-seq: '${seq}'
    body: '{"seq":${seq},"literal":"$${seq}"}'
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)
	require.Len(t, rb.Calls, 2)
	assert.Equal(t, `{"seq":${seq},"literal":"$${seq}"}`, string(rb.Calls[1].Body),
		"the raw templated body is kept as-is; substitution happens at run time")
}

// an unknown ${name} inside an @file body is a load error naming
// the resolved file path as well as the call.
func TestLoadReferenceUnknownInFileBody(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index-settings.json"), []byte(`{"number_of_replicas":${replicas}}`), 0o600))

	src := `
calls:
  - name: create_index
    path: /my-index
    body: '@index-settings.json'
`
	_, err := Load(strings.NewReader(src), dir)
	require.Error(t, err)
	assert.ErrorContains(t, err, "create_index")
	assert.ErrorContains(t, err, "replicas")
	assert.ErrorContains(t, err, filepath.Join(dir, "index-settings.json"))
}

func TestLoadBackoffJitter(t *testing.T) {
	src := `
calls:
  - name: c
    path: /x
    backoff-jitter: 0.25
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)
	assert.InDelta(t, 0.25, rb.Calls[0].Retry.Jitter, 1e-9)
}

// An invalid value in defaults: must fail the load attributed to defaults
// itself (line + offending key + underlying parse error), the same way an
// invalid per-call value is attributed to the call — never to whichever call
// happens not to override the bad key. loadDefaults decodes the block but,
// before this fix, never parsed/validated it: parsing only happened per-call,
// so an invalid default surfaced (misattributed) on the first call lacking an
// override, or not at all if every call happened to override it.
func TestLoadDefaultsInvalidFieldValues(t *testing.T) {
	tests := []struct {
		name        string
		defaultsKey string
		wantErrMsg  string
	}{
		{name: "bad duration", defaultsKey: "backoff-initial: 'notaduration'", wantErrMsg: "backoff-initial"},
		{name: "bad size", defaultsKey: "max-body-buffer: 'notasize'", wantErrMsg: "max-body-buffer"},
		{name: "bad backoff strategy", defaultsKey: "backoff: 'not-a-strategy'", wantErrMsg: `unknown backoff strategy "not-a-strategy"`},
		{name: "bad retry-when jq", defaultsKey: "retry-when: '.foo['", wantErrMsg: "retry-when"},
		{name: "bad success-when jq", defaultsKey: "success-when: '.foo['", wantErrMsg: "success-when"},
		{name: "missing body file inherited from defaults", defaultsKey: "body: '@missing-defaults-file.json'", wantErrMsg: "missing-defaults-file.json"},
		{name: "bad method", defaultsKey: "method: 'GET EXTRA'", wantErrMsg: "GET EXTRA"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "defaults:\n  " + tt.defaultsKey + "\ncalls:\n  - name: first_call\n    path: /x\n"
			_, err := Load(strings.NewReader(src), t.TempDir())
			require.Error(t, err)
			assert.Regexp(t, `defaults \(line \d+\)`, err.Error())
			assert.ErrorContains(t, err, tt.wantErrMsg)
			assert.NotContains(t, err.Error(), "first_call",
				"an invalid default must never be blamed on the call that didn't override it")
		})
	}
}

// The escape defect: an invalid default that every call happens to override
// must still fail the load. Before this fix it loaded cleanly — the bad
// default value was never parsed because no call fell through to it — so
// adding an innocent new call later that omits the override would suddenly
// surface an error that was always latent in the runbook.
func TestLoadDefaultsInvalidValueEscapesWhenEveryCallOverridesIt(t *testing.T) {
	src := `
defaults:
  backoff: 'not-a-strategy'

calls:
  - name: first_call
    path: /a
    backoff: exponential
  - name: second_call
    path: /b
    backoff: linear
`
	_, err := Load(strings.NewReader(src), "")
	require.Error(t, err, "an invalid default must fail the load even when every call overrides it")
	assert.ErrorContains(t, err, "defaults")
}

func TestLoadDefaultsForbiddenKeys(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "name forbidden in defaults", key: "name: foo"},
		{name: "path forbidden in defaults", key: "path: /foo"},
		{name: "capture forbidden in defaults", key: "capture:\n    x: '.x'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "defaults:\n  " + tt.key + "\ncalls:\n  - name: c\n    path: /x\n"
			_, err := Load(strings.NewReader(src), "")
			require.Error(t, err)
			assert.ErrorContains(t, err, "defaults")
		})
	}
}

func TestLoadFileSetsBaseDirFromFileDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "payload.json"), []byte(`{"in":"filedir"}`), 0o600))

	runbookPath := filepath.Join(dir, "run.yaml")
	src := "calls:\n  - name: c\n    path: /x\n    body: '@payload.json'\n"
	require.NoError(t, os.WriteFile(runbookPath, []byte(src), 0o600))

	rb, err := LoadFile(runbookPath)
	require.NoError(t, err)
	require.Len(t, rb.Calls, 1)
	assert.Equal(t, []byte(`{"in":"filedir"}`), rb.Calls[0].Body)
}
