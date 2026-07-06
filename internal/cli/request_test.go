package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

type recordedReq struct {
	method string
	path   string
	query  url.Values
	header http.Header
	body   []byte
	user   string
	pass   string
	authOK bool
}

type capture struct {
	mu   sync.Mutex
	reqs []recordedReq
}

func (c *capture) add(r *recordedReq) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, *r)
	return len(c.reqs) - 1
}

func (c *capture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func (c *capture) at(i int) recordedReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs[i]
}

// newServer replies with statuses[i] for the i-th request (clamped to the last)
// and a "body-<code>" payload, recording each request for assertions.
func newServer(t *testing.T, rec *capture, statuses []int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u, p, ok := r.BasicAuth()
		idx := rec.add(&recordedReq{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.Query(),
			header: r.Header.Clone(),
			body:   body,
			user:   u,
			pass:   p,
			authOK: ok,
		})
		code := statuses[len(statuses)-1]
		if idx < len(statuses) {
			code = statuses[idx]
		}
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "body-%d", code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func run(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCommand("test")
	var out, errb bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errb)
	if stdin != nil {
		root.SetIn(stdin)
	}
	err = root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func TestRequestSuccess(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})

	stdout, stderr, err := run(t, nil,
		"--endpoint", srv.URL,
		"-u", "admin", "--password", "secret",
		"-X", "POST",
		"--path", "_cluster/health",
		"--query", "size=5",
		"--header", "X-Custom: hi",
		"-d", `{"a":1}`,
	)
	require.NoError(t, err)
	assert.Equal(t, "body-200", stdout)
	assert.Empty(t, stderr)

	require.Equal(t, 1, rec.len())
	got := rec.at(0)
	assert.Equal(t, "POST", got.method)
	assert.Equal(t, "/_cluster/health", got.path)
	assert.Equal(t, "5", got.query.Get("size"))
	assert.Equal(t, "hi", got.header.Get("X-Custom"))
	assert.Equal(t, `{"a":1}`, string(got.body))
	assert.True(t, got.authOK)
	assert.Equal(t, "admin", got.user)
	assert.Equal(t, "secret", got.pass)
}

func TestRequestDefaultSingleAttempt(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusServiceUnavailable})

	stdout, _, err := run(t, nil,
		"--endpoint", srv.URL, "--path", "x",
	)
	require.Error(t, err)
	assert.Equal(t, 1, rec.len(), "no --retry means exactly one attempt")
	assert.Equal(t, "body-503", stdout, "the failing body is still printed")
}

func TestRequestRetryThenSuccess(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusOK,
	})

	stdout, _, err := run(t, nil,
		"--endpoint", srv.URL, "--path", "x",
		"--retry", "3", "--backoff-initial", "1ms",
	)
	require.NoError(t, err)
	assert.Equal(t, "body-200", stdout)
	assert.Equal(t, 3, rec.len())
}

func TestRequestRetriesExhausted(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusServiceUnavailable})

	stdout, _, err := run(t, nil,
		"--endpoint", srv.URL, "--path", "x",
		"--retry", "2", "--backoff-initial", "1ms",
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrRetriesExhausted)
	assert.Equal(t, 3, rec.len(), "1 initial + 2 retries")
	assert.Equal(t, "body-503", stdout)
}

func TestRequestAbortOn(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusConflict})

	stdout, _, err := run(t, nil,
		"--endpoint", srv.URL, "--path", "x",
		"--retry", "5", "--abort-on", "409",
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, retry.ErrTerminalStatus)
	assert.Equal(t, 1, rec.len(), "409 aborts immediately")
	assert.Equal(t, "body-409", stdout)
}

func TestRequestRetryReplaysBody(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusServiceUnavailable, http.StatusOK})

	_, _, err := run(t, nil,
		"--endpoint", srv.URL, "--path", "x",
		"-X", "POST", "-d", `{"x":1}`,
		"--retry", "1", "--backoff-initial", "1ms",
	)
	require.NoError(t, err)
	require.Equal(t, 2, rec.len())
	assert.Equal(t, `{"x":1}`, string(rec.at(1).body), "second attempt replays the body")
}

func TestRequestVerbose(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusOK,
	})

	stdout, stderr, err := run(t, nil,
		"--endpoint", srv.URL, "--path", "x",
		"--retry", "2", "--backoff-initial", "1ms", "--verbose",
	)
	require.NoError(t, err)
	assert.Equal(t, "body-200", stdout, "verbose output must not pollute stdout")
	assert.Contains(t, stderr, "attempt 1")
	assert.Contains(t, stderr, "attempt 2")
}

func TestRequestPasswordNeverLeaks(t *testing.T) {
	const pw = "topsecret"
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusServiceUnavailable})

	stdout, stderr, err := run(t, nil,
		"--endpoint", srv.URL, "-u", "admin", "--password", pw,
		"--path", "x", "--retry", "1", "--backoff-initial", "1ms", "--verbose",
	)
	require.Error(t, err)
	assert.NotContains(t, stdout, pw)
	assert.NotContains(t, stderr, pw)
	assert.NotContains(t, err.Error(), pw)
}

func TestRequestParseErrors(t *testing.T) {
	const endpoint = "http://localhost:9200"
	tests := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{name: "query without =", args: []string{"--query", "novalue"}, wantSub: "query"},
		{name: "header without :", args: []string{"--header", "novalue"}, wantSub: "header"},
		{name: "bad backoff", args: []string{"--backoff", "garbage"}, wantSub: "backoff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"--endpoint", endpoint, "--path", "x"}, tt.args...)
			_, _, err := run(t, nil, args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantSub)
		})
	}
}

func TestRequestHeaderValues(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})

	_, _, err := run(t, nil,
		"--endpoint", srv.URL, "--path", "x",
		"--header", "X-Colon: a:b",
		"--header", "X-Empty:",
	)
	require.NoError(t, err)
	got := rec.at(0)
	assert.Equal(t, "a:b", got.header.Get("X-Colon"), "colon in value preserved")
	_, ok := got.header["X-Empty"]
	assert.True(t, ok, "empty-valued header is sent")
	assert.Equal(t, "", got.header.Get("X-Empty"))
}

func TestRequestNonTTYNoPassword(t *testing.T) {
	t.Setenv("OPENSEARCH_PASSWORD", "")
	_, _, err := run(t, nil,
		"--endpoint", "http://localhost:9200", "-u", "admin", "--path", "x",
	)
	require.ErrorIs(t, err, config.ErrNoPassword)
}

func TestRequestBodyFromFile(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})

	path := filepath.Join(t.TempDir(), "body.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"f":1}`), 0o600))

	_, _, err := run(t, nil,
		"--endpoint", srv.URL, "--path", "x", "-X", "POST", "-d", "@"+path,
	)
	require.NoError(t, err)
	assert.Equal(t, `{"f":1}`, string(rec.at(0).body))
}

func TestRequestBodyFromStdin(t *testing.T) {
	var rec capture
	srv := newServer(t, &rec, []int{http.StatusOK})

	_, _, err := run(t, bytes.NewBufferString(`{"s":1}`),
		"--endpoint", srv.URL, "--path", "x", "-X", "POST", "-d", "@-",
	)
	require.NoError(t, err)
	assert.Equal(t, `{"s":1}`, string(rec.at(0).body))
}

func TestRequestEnvFile(t *testing.T) {
	t.Run("provides endpoint and credentials", func(t *testing.T) {
		var rec capture
		srv := newServer(t, &rec, []int{http.StatusOK})
		path := writeEnvFile(t, "OPENSEARCH_URL="+srv.URL+
			"\nOPENSEARCH_USERNAME=admin\nOPENSEARCH_PASSWORD=secret\n")

		_, _, err := run(t, nil, "--env-file", path, "--path", "_cluster/health")
		require.NoError(t, err)
		require.Equal(t, 1, rec.len())
		got := rec.at(0)
		assert.Equal(t, "admin", got.user)
		assert.Equal(t, "secret", got.pass)
	})

	t.Run("explicit endpoint overrides file", func(t *testing.T) {
		var rec capture
		srv := newServer(t, &rec, []int{http.StatusOK})
		path := writeEnvFile(t, "OPENSEARCH_URL=http://127.0.0.1:1\n"+
			"OPENSEARCH_USERNAME=admin\nOPENSEARCH_PASSWORD=secret\n")

		_, _, err := run(t, nil,
			"--env-file", path, "--endpoint", srv.URL, "--path", "x")
		require.NoError(t, err)
		assert.Equal(t, 1, rec.len())
	})

	t.Run("env file overrides process env", func(t *testing.T) {
		t.Setenv("OPENSEARCH_URL", "http://127.0.0.1:1")
		var rec capture
		srv := newServer(t, &rec, []int{http.StatusOK})
		path := writeEnvFile(t, "OPENSEARCH_URL="+srv.URL+"\n")

		_, _, err := run(t, nil, "--env-file", path, "--path", "x")
		require.NoError(t, err)
		assert.Equal(t, 1, rec.len())
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, _, err := run(t, nil,
			"--env-file", filepath.Join(t.TempDir(), "nope.env"), "--path", "x")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "env file")
	})
}

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "creds.env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestRequestBodySkeleton(t *testing.T) {
	// No --endpoint is set: the flag must short-circuit before any network use.
	t.Run("prints scaffold and sends no request", func(t *testing.T) {
		stdout, stderr, err := run(t, nil,
			"--path", "_search", "-X", "POST", "--body-skeleton")
		require.NoError(t, err)
		assert.True(t, json.Valid([]byte(stdout)), "stdout must be valid JSON: %s", stdout)
		assert.Contains(t, stdout, `"query"`)
		assert.Empty(t, stderr)
	})

	t.Run("no field template prints {} and a note", func(t *testing.T) {
		stdout, stderr, err := run(t, nil,
			"--path", "_cluster/health", "--body-skeleton")
		require.NoError(t, err)
		assert.Equal(t, "{}\n", stdout)
		assert.Contains(t, stderr, "no top-level field template")
	})
}
