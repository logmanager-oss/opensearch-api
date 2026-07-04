package osclient

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// stdinArg is the --body value requesting the body be read from stdin.
const stdinArg = "@-"

// defaultContentType is applied to requests that carry a body and set no
// explicit Content-Type.
const defaultContentType = "application/json"

// ErrNoStdin is returned when a stdin body is requested but no reader is given.
var ErrNoStdin = errors.New("stdin reader is nil")

// RequestSpec describes a raw OpenSearch request. Body is already buffered as
// bytes: the CLI resolves inline/@file/@- values (see ReadBody) before calling,
// which is required so retries can replay the body from req.GetBody. The body is
// held fully in memory, an intentional trade-off since retries must replay it.
type RequestSpec struct {
	Method  string
	Path    string
	Body    []byte
	HasBody bool
	Query   map[string]string
	Headers http.Header
}

// BuildRequest builds an absolute-URL request from endpoint + spec.Path,
// honouring the endpoint scheme (plain http included). It buffers the body so
// retries can replay it and applies caller headers with override semantics: an
// explicit Content-Type wins over the default application/json added when a body
// exists. Auth is not set here; it lives in the client transport.
//
//nolint:gocritic // hugeParam: RequestSpec is the documented value-typed public API.
func BuildRequest(endpoint string, spec RequestSpec) (*http.Request, error) {
	// Auth is added by the client transport, never via the URL: drop any
	// userinfo so a credential can never surface in the URL or an error message.
	endpoint = stripUserinfo(endpoint)

	// Join as strings so a query string embedded in spec.Path is preserved.
	full := strings.TrimRight(endpoint, "/") + "/" + strings.TrimLeft(spec.Path, "/")
	u, err := url.Parse(full)
	if err != nil {
		return nil, fmt.Errorf("parsing url %q: %w", full, urlErrReason(err))
	}
	if len(spec.Query) > 0 {
		q := u.Query() // preserves query params embedded in spec.Path
		for k, v := range spec.Query {
			q.Set(k, v) // spec.Query wins on duplicate keys
		}
		u.RawQuery = q.Encode()
	}

	var body io.Reader
	if spec.HasBody {
		body = bytes.NewReader(spec.Body)
	}

	req, err := http.NewRequest(spec.Method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("building request %s %s: %w", spec.Method, u.Redacted(), urlErrReason(err))
	}

	if spec.HasBody {
		// http.NewRequest already sets these for a *bytes.Reader; re-assign so
		// retry replay is explicit and independent of stdlib internals.
		buf := spec.Body
		req.ContentLength = int64(len(buf))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(buf)), nil
		}
		req.Header.Set("Content-Type", defaultContentType)
	}

	for key, values := range spec.Headers {
		req.Header.Del(key)
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}

	return req, nil
}

// ReadBody resolves a --body argument into buffered bytes: empty means no body
// (hasBody=false), "@-" reads all of stdin, "@path" reads a file (failing fast
// if missing), and anything else is the literal string. A value starting with
// "@" is always a file path (curl semantics). An empty "@file"/"@-" yields
// hasBody=true with a zero-length body — intentionally asymmetric with arg=="".
// The body is fully buffered in memory so retries can replay it.
func ReadBody(arg string, stdin io.Reader) (data []byte, hasBody bool, err error) {
	if arg == "" {
		return nil, false, nil
	}

	if arg == stdinArg {
		if stdin == nil {
			return nil, false, ErrNoStdin
		}
		data, err = io.ReadAll(stdin)
		if err != nil {
			return nil, false, fmt.Errorf("reading body from stdin: %w", err)
		}
		return data, true, nil
	}

	if strings.HasPrefix(arg, "@") {
		path := arg[1:]
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, false, fmt.Errorf("reading body file %q: %w", path, err)
		}
		return data, true, nil
	}

	return []byte(arg), true, nil
}

// stripUserinfo removes any userinfo (user:password@) from a raw URL so a
// credential can never surface in the request URL or an error message.
func stripUserinfo(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	at := strings.IndexByte(rest, '@')
	slash := strings.IndexByte(rest, '/')
	if at < 0 || (slash >= 0 && at > slash) {
		return raw
	}
	return raw[:i+3] + rest[at+1:]
}

// urlErrReason returns the inner reason of a *url.Error, dropping its URL field
// which may contain credentials.
func urlErrReason(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
