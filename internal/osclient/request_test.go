package osclient

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadBody(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "body.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"file":true}`), 0o600))

	tests := []struct {
		name     string
		arg      string
		stdin    io.Reader
		wantData string
		wantBody bool
		wantErr  bool
	}{
		{name: "empty means no body", arg: "", wantBody: false},
		{name: "inline literal", arg: `{"a":1}`, wantData: `{"a":1}`, wantBody: true},
		{name: "stdin via @-", arg: "@-", stdin: strings.NewReader(`{"in":1}`), wantData: `{"in":1}`, wantBody: true},
		{name: "file via @path", arg: "@" + file, wantData: `{"file":true}`, wantBody: true},
		{name: "missing file errors", arg: "@" + filepath.Join(dir, "nope.json"), wantErr: true},
		{name: "empty @- yields zero-length body", arg: "@-", stdin: strings.NewReader(""), wantData: "", wantBody: true},
		{name: "nil stdin errors", arg: "@-", stdin: nil, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, hasBody, err := ReadBody(tt.arg, tt.stdin)
			if tt.wantErr {
				require.Error(t, err)
				assert.False(t, hasBody)
				assert.Nil(t, data)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantBody, hasBody)
			if tt.wantBody {
				assert.Equal(t, tt.wantData, string(data))
			}
		})
	}
}

const testEndpoint = "http://localhost:9200"

func TestBuildRequest_AbsoluteURLJoined(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		path     string
		wantURL  string
	}{
		{name: "simple join", endpoint: "http://localhost:9200", path: "_cluster/health", wantURL: "http://localhost:9200/_cluster/health"},
		{name: "endpoint trailing slash", endpoint: "http://localhost:9200/", path: "_cluster/health", wantURL: "http://localhost:9200/_cluster/health"},
		{name: "path leading slash", endpoint: "http://localhost:9200", path: "/_cluster/health", wantURL: "http://localhost:9200/_cluster/health"},
		{name: "https scheme honoured", endpoint: "https://os.example:9200", path: "_search", wantURL: "https://os.example:9200/_search"},
		{name: "query embedded in path preserved", endpoint: "http://localhost:9200", path: "_search?scroll=1m", wantURL: "http://localhost:9200/_search?scroll=1m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := BuildRequest(tt.endpoint, RequestSpec{Method: http.MethodGet, Path: tt.path})
			require.NoError(t, err)
			assert.Equal(t, tt.wantURL, req.URL.String())
		})
	}
}

func TestBuildRequest_QueryMerge(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		query map[string]string
		want  map[string]string
	}{
		{
			name:  "path query merged with spec query",
			path:  "_search?scroll=1m",
			query: map[string]string{"pretty": "true"},
			want:  map[string]string{"scroll": "1m", "pretty": "true"},
		},
		{
			name:  "spec query wins on duplicate key",
			path:  "_search?pretty=false",
			query: map[string]string{"pretty": "true"},
			want:  map[string]string{"pretty": "true"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := BuildRequest(testEndpoint, RequestSpec{
				Method: http.MethodGet,
				Path:   tt.path,
				Query:  tt.query,
			})
			require.NoError(t, err)
			got := req.URL.Query()
			for k, v := range tt.want {
				assert.Equal(t, v, got.Get(k), k)
			}
		})
	}
}

func TestBuildRequest_UserinfoNotLeakedInError(t *testing.T) {
	const password = "s3cr3t"
	// An invalid method forces http.NewRequest to fail after the URL is built.
	_, err := BuildRequest("https://user:"+password+"@localhost:9200", RequestSpec{
		Method: "BAD METHOD",
		Path:   "_search",
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), password)
}

func TestBuildRequest_UserinfoStrippedFromURL(t *testing.T) {
	req, err := BuildRequest("https://user:s3cr3t@localhost:9200", RequestSpec{
		Method: http.MethodGet,
		Path:   "_search",
	})
	require.NoError(t, err)
	assert.Nil(t, req.URL.User)
	assert.NotContains(t, req.URL.String(), "s3cr3t")
}

func TestBuildRequest_DefaultContentType(t *testing.T) {
	req, err := BuildRequest(testEndpoint, RequestSpec{
		Method:  http.MethodPost,
		Path:    "_bulk",
		Body:    []byte(`{"a":1}`),
		HasBody: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Len(t, req.Header.Values("Content-Type"), 1)
}

func TestBuildRequest_ExplicitContentTypeOverrides(t *testing.T) {
	headers := http.Header{}
	headers.Set("Content-Type", "text/plain")
	req, err := BuildRequest(testEndpoint, RequestSpec{
		Method:  http.MethodPost,
		Path:    "_bulk",
		Body:    []byte("plain"),
		HasBody: true,
		Headers: headers,
	})
	require.NoError(t, err)
	assert.Equal(t, "text/plain", req.Header.Get("Content-Type"))
	assert.Len(t, req.Header.Values("Content-Type"), 1)
}

func TestBuildRequest_NoBodyNoContentType(t *testing.T) {
	req, err := BuildRequest(testEndpoint, RequestSpec{
		Method: http.MethodGet,
		Path:   "_cluster/health",
	})
	require.NoError(t, err)
	assert.Empty(t, req.Header.Get("Content-Type"))
	assert.Nil(t, req.Body)
}

func TestBuildRequest_QueryReflectedOnURL(t *testing.T) {
	req, err := BuildRequest(testEndpoint, RequestSpec{
		Method: http.MethodGet,
		Path:   "_search",
		Query:  map[string]string{"pretty": "true", "size": "5"},
	})
	require.NoError(t, err)
	q := req.URL.Query()
	assert.Equal(t, "true", q.Get("pretty"))
	assert.Equal(t, "5", q.Get("size"))
}

func TestBuildRequest_QueryValuesURLEncoded(t *testing.T) {
	req, err := BuildRequest(testEndpoint, RequestSpec{
		Method: http.MethodGet,
		Path:   "_search",
		Query:  map[string]string{"q": "a b&c"},
	})
	require.NoError(t, err)
	assert.Equal(t, "a b&c", req.URL.Query().Get("q"))
	assert.NotContains(t, req.URL.RawQuery, "a b&c")
	assert.Contains(t, req.URL.RawQuery, "a+b%26c")
}

func TestBuildRequest_GetBodyReplaysFreshReader(t *testing.T) {
	const payload = `{"replay":true}`
	req, err := BuildRequest(testEndpoint, RequestSpec{
		Method:  http.MethodPut,
		Path:    "idx/_doc/1",
		Body:    []byte(payload),
		HasBody: true,
	})
	require.NoError(t, err)
	require.NotNil(t, req.GetBody)
	assert.Equal(t, int64(len(payload)), req.ContentLength)

	first, err := req.GetBody()
	require.NoError(t, err)
	firstData, err := io.ReadAll(first)
	require.NoError(t, err)
	require.NoError(t, first.Close())
	assert.Equal(t, payload, string(firstData))

	second, err := req.GetBody()
	require.NoError(t, err)
	secondData, err := io.ReadAll(second)
	require.NoError(t, err)
	require.NoError(t, second.Close())
	assert.Equal(t, payload, string(secondData))

	// The request's own Body remains independently readable.
	bodyData, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, string(bodyData))
}
