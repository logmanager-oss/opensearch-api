package osclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedRequest struct {
	method string
	query  string
	body   string
	tenant string
}

// upsertServer answers GET with getStatus/getBody and PUT with putStatus. It records every request.
func upsertServer(t *testing.T, getStatus int, getBody string, putStatus int) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, recordedRequest{
			method: r.Method, query: r.URL.RawQuery, body: string(b), tenant: r.Header.Get("X-Tenant"),
		})
		if r.Method == http.MethodGet {
			w.WriteHeader(getStatus)
			_, _ = w.Write([]byte(getBody))
			return
		}
		w.WriteHeader(putStatus)
		_, _ = w.Write([]byte("put-response"))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestUpsertAttempt(t *testing.T) {
	const putBody = `{"policy":{"a":1}}`
	tests := []struct {
		name        string
		getStatus   int
		getBody     string
		putStatus   int
		unchanged   func([]byte) bool
		wantStatus  int
		wantBody    string
		wantErr     error
		wantMethods []string
		wantPutQ    string
	}{
		{
			name: "get 404 gives plain put", getStatus: 404, putStatus: 201,
			wantStatus: 201, wantBody: "put-response", wantMethods: []string{"GET", "PUT"}, wantPutQ: "refresh=true",
		},
		{
			name: "get 200 gives conditional put", getStatus: 200, getBody: `{"_seq_no":7,"_primary_term":2}`, putStatus: 200,
			wantStatus: 200, wantBody: "put-response", wantMethods: []string{"GET", "PUT"}, wantPutQ: "if_primary_term=2&if_seq_no=7&refresh=true",
		},
		{
			name: "seq_no missing", getStatus: 200, getBody: `{"_primary_term":2}`,
			wantErr: ErrNoSeqNo, wantMethods: []string{"GET"},
		},
		{
			name: "primary_term missing", getStatus: 200, getBody: `{"_seq_no":7}`,
			wantErr: ErrNoSeqNo, wantMethods: []string{"GET"},
		},
		{
			name: "seq_no is a string", getStatus: 200, getBody: `{"_seq_no":"7","_primary_term":2}`,
			wantErr: ErrNoSeqNo, wantMethods: []string{"GET"},
		},
		{
			name: "non-json body", getStatus: 200, getBody: `<html>`,
			wantErr: ErrNoSeqNo, wantMethods: []string{"GET"},
		},
		{
			name: "get 503 returned without put", getStatus: 503, getBody: "down",
			wantStatus: 503, wantBody: "down", wantMethods: []string{"GET"},
		},
		{
			name: "put 409 returned", getStatus: 200, getBody: `{"_seq_no":7,"_primary_term":2}`, putStatus: 409,
			wantStatus: 409, wantBody: "put-response", wantMethods: []string{"GET", "PUT"}, wantPutQ: "if_primary_term=2&if_seq_no=7&refresh=true",
		},
		{
			name: "unchanged true returns get response without put", getStatus: 200, getBody: `{"_seq_no":7,"_primary_term":2,"x":1}`,
			unchanged:  func(b []byte) bool { return string(b) == `{"_seq_no":7,"_primary_term":2,"x":1}` },
			wantStatus: 200, wantBody: `{"_seq_no":7,"_primary_term":2,"x":1}`, wantMethods: []string{"GET"},
		},
		{
			name: "unchanged false proceeds to conditional put", getStatus: 200, getBody: `{"_seq_no":7,"_primary_term":2}`, putStatus: 200,
			unchanged:  func([]byte) bool { return false },
			wantStatus: 200, wantBody: "put-response", wantMethods: []string{"GET", "PUT"}, wantPutQ: "if_primary_term=2&if_seq_no=7&refresh=true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := upsertServer(t, tt.getStatus, tt.getBody, tt.putStatus)
			spec := RequestSpec{
				Method: http.MethodPut, Path: "_plugins/_ism/policies/p",
				Body: []byte(putBody), HasBody: true, Query: map[string]string{"refresh": "true"},
				Headers: http.Header{"X-Tenant": {"t1"}},
			}
			attempt, err := Upsert(srv.Client(), srv.URL, spec, tt.unchanged) //nolint:bodyclose // the attempt's response is closed where it is called
			require.NoError(t, err)

			resp, err := attempt(context.Background())
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				assert.Equal(t, tt.wantStatus, resp.StatusCode)
				b, _ := io.ReadAll(resp.Body)
				assert.Equal(t, tt.wantBody, string(b))
			}

			var methods []string
			for _, r := range *got {
				methods = append(methods, r.method)
				assert.Equal(t, "t1", r.tenant)
				if r.method == http.MethodGet {
					assert.Empty(t, r.body, "GET must not carry the PUT body")
					assert.Equal(t, "refresh=true", r.query)
					continue
				}
				assert.Equal(t, putBody, r.body)
				assert.Equal(t, tt.wantPutQ, r.query)
			}
			assert.Equal(t, tt.wantMethods, methods)
		})
	}
}

func TestUpsertReplaysBody(t *testing.T) {
	srv, got := upsertServer(t, 404, "", 200)
	spec := RequestSpec{Method: http.MethodPut, Path: "p", Body: []byte(`{"a":1}`), HasBody: true}
	attempt, err := Upsert(srv.Client(), srv.URL, spec, nil) //nolint:bodyclose // the attempt's response is closed where it is called
	require.NoError(t, err)

	for range 2 {
		resp, err := attempt(context.Background())
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	var puts []string
	for _, r := range *got {
		if r.method == http.MethodPut {
			puts = append(puts, r.body)
		}
	}
	assert.Equal(t, []string{`{"a":1}`, `{"a":1}`}, puts)
}

func TestReplay(t *testing.T) {
	tests := []struct {
		name    string
		spec    RequestSpec
		wantLen int64
	}{
		{name: "body replayed", spec: RequestSpec{Method: http.MethodPost, Path: "p", Body: []byte(`{"a":1}`), HasBody: true}, wantLen: 7},
		{name: "bodiless stays bodiless", spec: RequestSpec{Method: http.MethodGet, Path: "p"}, wantLen: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bodies []string
			var lengths []int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(b))
				lengths = append(lengths, r.ContentLength)
			}))
			t.Cleanup(srv.Close)

			req, err := BuildRequest(srv.URL, tt.spec)
			require.NoError(t, err)
			attempt := Replay(srv.Client(), req) //nolint:bodyclose // the attempt's response is closed where it is called

			for range 2 {
				resp, err := attempt(context.Background())
				require.NoError(t, err)
				_ = resp.Body.Close()
			}

			want := string(tt.spec.Body)
			assert.Equal(t, []string{want, want}, bodies)
			assert.Equal(t, []int64{tt.wantLen, tt.wantLen}, lengths)
		})
	}
}
