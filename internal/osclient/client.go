// Package osclient builds a standard-library HTTP client and raw requests for a
// single OpenSearch endpoint. It is standalone: the CLI maps its own
// configuration onto Options.
package osclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// ErrNoEndpoint is returned when Options.Endpoint is empty.
var ErrNoEndpoint = errors.New("endpoint is required")

// ErrInvalidEndpoint is returned when Options.Endpoint has no scheme or host.
var ErrInvalidEndpoint = errors.New("invalid endpoint")

const (
	redactedPassword = "***"
	userAgent        = "osapi"

	defaultHTTPPort  = "80"
	defaultHTTPSPort = "443"
)

// Options configures New. Warn receives human-readable warnings; a nil Warn
// discards them. The endpoint scheme is honoured as given (plain http:// is
// allowed, not forced to https). Prefer Username/Password over URL userinfo in
// the endpoint (https://user:pass@host): userinfo can surface in transport
// errors.
type Options struct {
	Endpoint   string
	Username   string
	Password   string
	CACertPath string
	Insecure   bool
	Warn       io.Writer
}

// optionsAlias drops the String/GoString methods to avoid infinite recursion
// while formatting a redacted copy.
type optionsAlias Options

// String redacts the password so it never leaks through %v/%+v/%s formatting.
//
//nolint:gocritic // value receiver required to satisfy fmt.Stringer on an Options value.
func (o Options) String() string {
	if o.Password != "" {
		o.Password = redactedPassword
	}
	return fmt.Sprintf("%v", optionsAlias(o))
}

// GoString redacts the password so it never leaks through %#v formatting.
//
//nolint:gocritic // value receiver required to satisfy fmt.GoStringer on an Options value.
func (o Options) GoString() string {
	if o.Password != "" {
		o.Password = redactedPassword
	}
	return fmt.Sprintf("%#v", optionsAlias(o))
}

// New builds an *http.Client for opts. TLS precedence mirrors an insecure curl:
// Insecure wins over CACertPath (a warning is emitted when both are set). A bad
// CA (empty/garbage PEM) is an error, never a silent fallback. The password is
// never logged.
//
// Notes: the client has no Timeout — cancellation is driven by the request
// context (the retry engine owns it). Request bodies are buffered fully in
// memory so they can be replayed on retry (see BuildRequest). Redirects are
// followed, but Basic auth is only ever sent to the configured origin (scheme +
// host + port), never to a redirect target on another origin.
//
//nolint:gocritic // hugeParam: Options is the documented value-typed public API.
func New(opts Options) (*http.Client, error) {
	if opts.Endpoint == "" {
		return nil, ErrNoEndpoint
	}
	origin, err := parseOrigin(opts.Endpoint)
	if err != nil {
		return nil, err
	}

	tlsCfg, err := buildTLSConfig(opts)
	if err != nil {
		return nil, err
	}

	// OpenSearch only accepts Basic auth when both credentials are non-empty; a
	// username with no password would be sent anonymously with no other signal.
	if opts.Username != "" && opts.Password == "" && opts.Warn != nil {
		_, _ = fmt.Fprintln(opts.Warn, "warning: username set without a password; request will be sent unauthenticated")
	}

	transport := &authTransport{
		base:     &http.Transport{TLSClientConfig: tlsCfg},
		username: opts.Username,
		password: opts.Password,
		origin:   origin,
	}
	return &http.Client{Transport: transport}, nil
}

// origin identifies the endpoint that credentials may be sent to.
type origin struct {
	scheme string
	host   string
	port   string
}

// parseOrigin validates endpoint and extracts its normalized origin.
func parseOrigin(endpoint string) (origin, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return origin{}, fmt.Errorf("%w %q: %w", ErrInvalidEndpoint, stripUserinfo(endpoint), urlErrReason(err))
	}
	if u.Scheme == "" || u.Hostname() == "" {
		return origin{}, fmt.Errorf("%w %q: need scheme and host (e.g. https://host:9200)", ErrInvalidEndpoint, stripUserinfo(endpoint))
	}
	return origin{
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
		port:   effectivePort(u),
	}, nil
}

// buildTLSConfig derives the TLS config from opts, or nil to use the default.
//
//nolint:gocritic // hugeParam: Options mirrors New's documented value API.
func buildTLSConfig(opts Options) (*tls.Config, error) {
	if opts.Insecure {
		if opts.CACertPath != "" && opts.Warn != nil {
			_, _ = fmt.Fprintln(opts.Warn, "warning: --insecure set, ignoring CA cert and skipping TLS verification")
		}
		return &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // insecure verification explicitly requested
			MinVersion:         tls.VersionTLS12,
		}, nil
	}

	if opts.CACertPath == "" {
		return nil, nil
	}

	pem, err := os.ReadFile(opts.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert %q: %w", opts.CACertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("adding CA cert %q: no valid certificate found", opts.CACertPath)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// authTransport adds a User-Agent and, only for requests to the configured
// origin, Basic auth. Per the RoundTripper contract it clones the request before
// mutating instead of touching the caller's request. Origin-gating the auth
// prevents leaking credentials to a redirect target on another host/scheme.
type authTransport struct {
	base     http.RoundTripper
	username string
	password string
	origin   origin
}

var _ http.RoundTripper = (*authTransport)(nil)

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if clone.Header.Get("User-Agent") == "" {
		clone.Header.Set("User-Agent", userAgent)
	}
	if t.username != "" && t.password != "" && t.origin.matches(clone.URL) {
		clone.SetBasicAuth(t.username, t.password)
	}
	return t.base.RoundTrip(clone)
}

// matches reports whether u has the same scheme, host and effective port.
func (o origin) matches(u *url.URL) bool {
	return strings.EqualFold(u.Scheme, o.scheme) &&
		strings.EqualFold(u.Hostname(), o.host) &&
		effectivePort(u) == o.port
}

// effectivePort returns the URL port, defaulting per scheme when absent.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return defaultHTTPSPort
	case "http":
		return defaultHTTPPort
	default:
		return ""
	}
}
