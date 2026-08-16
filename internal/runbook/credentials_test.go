package runbook

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

func mapLookup(m map[string]string) config.EnvLookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestLoadCredentials(t *testing.T) {
	src := `
defaults:
  credentials:
    username: admin
    password: '${secret:OS_PASSWORD}'

calls:
  - name: c
    path: /x
`
	rb, err := Load(strings.NewReader(src), "")
	require.NoError(t, err)
	require.NotNil(t, rb.Credentials)
	assert.Equal(t, "admin", rb.Credentials.Username)
	assert.Equal(t, "${secret:OS_PASSWORD}", rb.Credentials.Password,
		"a secret reference stays verbatim after Load; only Resolve touches the environment")
}

func TestLoadWithoutCredentials(t *testing.T) {
	src := `
calls:
  - name: c
    path: /x
`
	rb, err := Load(strings.NewReader(src), "")

	require.NoError(t, err)
	assert.Nil(t, rb.Credentials, "an absent block must stay nil: callers switch on it")
}

// An explicit non-string tag makes yaml.v3 quote the scalar in its decode
// error. parseCredentials must not route the password through that path.
func TestLoadCredentialsExplicitTagDoesNotEchoValue(t *testing.T) {
	src := `
defaults:
  credentials:
    username: admin
    password: !!int hunter2
calls:
  - name: c
    path: /x
`
	rb, err := Load(strings.NewReader(src), "")

	require.NoError(t, err)
	assert.Equal(t, "hunter2", rb.Credentials.Password)
}

func TestLoadPerCallCredentialsKeyRejected(t *testing.T) {
	src := `
calls:
  - name: c
    path: /x
    credentials:
      username: admin
      password: secret
`
	_, err := Load(strings.NewReader(src), "")
	require.Error(t, err)
	assert.ErrorContains(t, err, `call "c" (line`)
	assert.ErrorContains(t, err, `unknown key "credentials"`)
}

func TestLoadCredentialsShapeErrors(t *testing.T) {
	tests := []struct {
		name       string
		src        string
		wantErrMsg []string
	}{
		{
			name: "non-mapping node",
			src: `
defaults:
  credentials: admin
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{"must be a mapping", "got a scalar"},
		},
		{
			name: "unknown nested key",
			src: `
defaults:
  credentials:
    username: admin
    password: secret
    token: abc
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{`unknown key "token"`},
		},
		{
			name: "missing username",
			src: `
defaults:
  credentials:
    password: secret
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{`missing required key "username"`},
		},
		{
			name: "missing password",
			src: `
defaults:
  credentials:
    username: admin
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{`missing required key "password"`},
		},
		{
			// A key that is present but empty must not read as missing.
			name: "empty literal value",
			src: `
defaults:
  credentials:
    username: admin
    password: ''
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{`"password" must not be empty`},
		},
		{
			// parseCredentials bypasses node.Decode, so it must reject a
			// repeated key itself instead of letting the first one win.
			name: "duplicate key",
			src: `
defaults:
  credentials:
    username: admin
    password: stale
    password: current
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{`duplicate key "password"`},
		},
		{
			name: "non-scalar value",
			src: `
defaults:
  credentials:
    username: admin
    password:
      nested: secret
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{`"password" must be a string`, "got a mapping"},
		},
		{
			name: "reference without secret prefix",
			src: `
defaults:
  credentials:
    username: admin
    password: '${foo}'
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{"only ${secret:NAME} references are supported in credentials"},
		},
		{
			// Every credentials error names its field, including this one,
			// which scanTemplate raises before any name is parsed.
			name: "unterminated reference names its field",
			src: `
defaults:
  credentials:
    username: admin
    password: '${secret:OS_P'
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{"password", "unterminated"},
		},
		{
			// The username is syntax-checked too, not only the password.
			name: "bad reference in username",
			src: `
defaults:
  credentials:
    username: '${foo}'
    password: secret
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{"username", "only ${secret:NAME} references are supported in credentials"},
		},
		{
			name: "empty secret name",
			src: `
defaults:
  credentials:
    username: admin
    password: '${secret:}'
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{"only ${secret:NAME} references are supported in credentials"},
		},
		{
			name: "secret name starting with a digit",
			src: `
defaults:
  credentials:
    username: admin
    password: '${secret:1BAD}'
calls:
  - name: c
    path: /x
`,
			wantErrMsg: []string{"only ${secret:NAME} references are supported in credentials"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.src), "")
			require.Error(t, err)
			assert.Regexp(t, `defaults \(line \d+\): credentials`, err.Error())
			for _, want := range tt.wantErrMsg {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestCredentialsResolve(t *testing.T) {
	tests := []struct {
		name         string
		creds        Credentials
		env          map[string]string
		wantUsername string
		wantPassword string
		wantErrMsg   string
	}{
		{
			name:         "literal pass-through",
			creds:        Credentials{Username: "admin", Password: "hunter2"},
			wantUsername: "admin",
			wantPassword: "hunter2",
		},
		{
			name:         "secret reference resolves",
			creds:        Credentials{Username: "admin", Password: "${secret:OS_PASSWORD}"},
			env:          map[string]string{"OS_PASSWORD": "hunter2"},
			wantUsername: "admin",
			wantPassword: "hunter2",
		},
		{
			name:         "mixed literal and reference",
			creds:        Credentials{Username: "svc-${secret:OS_SUFFIX}", Password: "hunter2"},
			env:          map[string]string{"OS_SUFFIX": "42"},
			wantUsername: "svc-42",
			wantPassword: "hunter2",
		},
		{
			name:         "escaped dollar-brace yields a literal ${",
			creds:        Credentials{Username: "admin", Password: `$${literal}`},
			wantUsername: "admin",
			wantPassword: "${literal}",
		},
		{
			name:       "unset variable errors naming it",
			creds:      Credentials{Username: "admin", Password: "${secret:OS_PASSWORD}"},
			env:        map[string]string{},
			wantErrMsg: "OS_PASSWORD",
		},
		{
			name:       "empty variable errors the same way",
			creds:      Credentials{Username: "admin", Password: "${secret:OS_PASSWORD}"},
			env:        map[string]string{"OS_PASSWORD": ""},
			wantErrMsg: "OS_PASSWORD",
		},
		{
			// The username resolves before the password fails, so a real
			// secret value is in hand when the error is built.
			name:       "a resolved secret does not leak into a later error",
			creds:      Credentials{Username: "${secret:OS_USER}", Password: "${secret:OS_MISSING}"},
			env:        map[string]string{"OS_USER": "hunter2"},
			wantErrMsg: "OS_MISSING",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := tt.creds.Resolve(mapLookup(tt.env))
			if tt.wantErrMsg != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErrMsg)
				assert.NotContains(t, err.Error(), "hunter2", "no secret value leaks into the error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantUsername, resolved.Username)
			assert.Equal(t, tt.wantPassword, resolved.Password)
		})
	}
}

// A nil lookup means "nothing is set" (config.EnvLookup's contract), so a
// reference must fail loudly rather than resolve to an empty credential.
func TestCredentialsResolveNilLookup(t *testing.T) {
	_, err := (&Credentials{Username: "admin", Password: "${secret:OS_PASSWORD}"}).Resolve(nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "OS_PASSWORD")
}

func TestCredentialsRedaction(t *testing.T) {
	c := Credentials{Username: "admin", Password: "hunter2"}

	assert.NotContains(t, c.String(), "hunter2")
	assert.NotContains(t, fmt.Sprintf("%+v", c), "hunter2")
	assert.NotContains(t, fmt.Sprintf("%#v", c), "hunter2")
}
