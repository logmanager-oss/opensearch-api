package osclient

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testUser = "admin"

func TestNew_EmptyEndpointErrors(t *testing.T) {
	_, err := New(Options{})
	require.ErrorIs(t, err, ErrNoEndpoint)
}

func TestNew_InsecureConnectsToTLSServer(t *testing.T) {
	var gotAuthUser, gotUserAgent string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthUser = basicAuthUser(t, r)
		gotUserAgent = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := New(Options{
		Endpoint: srv.URL,
		Username: testUser,
		Password: "s3cret",
		Insecure: true,
	})
	require.NoError(t, err)

	resp := do(t, client, srv.URL)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, testUser, gotAuthUser)
	assert.Equal(t, userAgent, gotUserAgent)
}

func TestNew_ValidCASucceeds(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	caPath := filepath.Join(t.TempDir(), "valid-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(caPath, pemBytes, 0o600))

	client, err := New(Options{Endpoint: srv.URL, CACertPath: caPath})
	require.NoError(t, err)

	resp := do(t, client, srv.URL)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNew_WrongCAWithoutInsecureFails(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	caPath := filepath.Join(t.TempDir(), "unrelated-ca.pem")
	require.NoError(t, os.WriteFile(caPath, unrelatedCAPEM(t), 0o600))

	client, err := New(Options{Endpoint: srv.URL, CACertPath: caPath})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/", http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "certificate")
}

func TestNew_MissingCACertErrors(t *testing.T) {
	_, err := New(Options{
		Endpoint:   "https://localhost:9200",
		CACertPath: filepath.Join(t.TempDir(), "nope.pem"),
	})
	require.Error(t, err)
}

func TestNew_InvalidCACertErrors(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "empty file", content: nil},
		{name: "garbage non-PEM", content: []byte("not a certificate")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caPath := filepath.Join(t.TempDir(), "ca.pem")
			require.NoError(t, os.WriteFile(caPath, tt.content, 0o600))

			_, err := New(Options{Endpoint: "https://localhost:9200", CACertPath: caPath})
			require.Error(t, err)
		})
	}
}

func TestNew_InsecureWithCACertWarnsAndConnects(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, unrelatedCAPEM(t), 0o600))

	const password = "s3cret"
	var warn bytes.Buffer
	client, err := New(Options{
		Endpoint:   srv.URL,
		Username:   testUser,
		Password:   password,
		CACertPath: caPath,
		Insecure:   true,
		Warn:       &warn,
	})
	require.NoError(t, err)

	resp := do(t, client, srv.URL)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, warn.String())
	assert.NotContains(t, warn.String(), password)
}

func TestNew_PlainHTTPEndpointHonoured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	require.True(t, strings.HasPrefix(srv.URL, "http://"))

	client, err := New(Options{Endpoint: srv.URL})
	require.NoError(t, err)

	resp := do(t, client, srv.URL)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNew_UsernameWithoutPasswordWarnsAndSendsAnonymous(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var warn bytes.Buffer
	client, err := New(Options{Endpoint: srv.URL, Username: testUser, Warn: &warn})
	require.NoError(t, err)

	resp := do(t, client, srv.URL)
	defer func() { _ = resp.Body.Close() }()

	assert.Contains(t, warn.String(), "unauthenticated")
	assert.Empty(t, gotAuth, "no Authorization header without a password")
}

func TestNew_AuthTransportDoesNotMutateRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := New(Options{Endpoint: srv.URL, Username: testUser, Password: "s3cret"})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/", http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// The RoundTripper must clone, leaving the caller's request untouched.
	assert.Empty(t, req.Header.Get("Authorization"))
	assert.Empty(t, req.Header.Get("User-Agent"))
}

func TestNew_InvalidEndpointErrors(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "scheme-less host:port", endpoint: "localhost:9200"},
		{name: "bare host", endpoint: "localhost"},
		{name: "scheme only", endpoint: "https://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Options{Endpoint: tt.endpoint})
			require.ErrorIs(t, err, ErrInvalidEndpoint)
		})
	}
}

func TestNew_AuthNotLeakedOnCrossOriginRedirect(t *testing.T) {
	var targetAuth, originAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, target.URL+"/", http.StatusFound)
	}))
	defer source.Close()

	client, err := New(Options{Endpoint: source.URL, Username: testUser, Password: "s3cret"})
	require.NoError(t, err)

	resp := do(t, client, source.URL)
	defer func() { _ = resp.Body.Close() }()

	assert.NotEmpty(t, originAuth, "same-origin request must carry auth")
	assert.Empty(t, targetAuth, "auth must NOT be sent to a cross-origin redirect target")
}

func TestNew_CallerUserAgentSurvives(t *testing.T) {
	const customUA = "custom-agent/1.0"
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := New(Options{Endpoint: srv.URL})
	require.NoError(t, err)

	headers := http.Header{}
	headers.Set("User-Agent", customUA)
	req, err := BuildRequest(srv.URL, RequestSpec{Method: http.MethodGet, Path: "_cluster/health", Headers: headers})
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, customUA, gotUA)
}

func TestOptions_RedactsPassword(t *testing.T) {
	const password = "topsecret"
	opts := Options{
		Endpoint: "https://os.example:9200",
		Username: testUser,
		Password: password,
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(format, opts)
		assert.NotContains(t, out, password, format)
		assert.Contains(t, out, redactedPassword, format)
		assert.Contains(t, out, "os.example", format)
		assert.Contains(t, out, testUser, format)
	}
}

func do(t *testing.T, client *http.Client, endpoint string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"/", http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

func basicAuthUser(t *testing.T, r *http.Request) string {
	t.Helper()
	const prefix = "Basic "
	h := r.Header.Get("Authorization")
	require.True(t, strings.HasPrefix(h, prefix))
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, prefix))
	require.NoError(t, err)
	user, _, ok := strings.Cut(string(raw), ":")
	require.True(t, ok)
	return user
}

// unrelatedCAPEM returns a valid, self-signed CA certificate unrelated to any
// httptest server, so verifying against it fails.
func unrelatedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
