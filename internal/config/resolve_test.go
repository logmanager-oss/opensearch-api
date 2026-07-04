package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// changedSet builds a Changed predicate from a set of field names.
func changedSet(fields ...string) func(string) bool {
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	return func(field string) bool { return set[field] }
}

func TestResolveDefaults(t *testing.T) {
	got, err := Resolve(Sources{})
	require.NoError(t, err)
	assert.Equal(t, Defaults(), got)
}

func TestResolveEndpointPrecedence(t *testing.T) {
	prof := &Profile{Endpoint: "https://profile"}
	env := mapLookup(map[string]string{"OPENSEARCH_URL": "https://env"})

	tests := []struct {
		name    string
		sources Sources
		want    string
	}{
		{
			name: "flag beats all",
			sources: Sources{
				Flags:   Config{Endpoint: "https://flag"},
				Changed: changedSet(FieldEndpoint),
				Env:     env, UseEnv: true, Profile: prof,
			},
			want: "https://flag",
		},
		{
			name:    "env beats profile",
			sources: Sources{Env: env, UseEnv: true, Profile: prof},
			want:    "https://env",
		},
		{
			name:    "env ignored when UseEnv false",
			sources: Sources{Env: env, UseEnv: false, Profile: prof},
			want:    "https://profile",
		},
		{
			name:    "profile beats default",
			sources: Sources{Profile: prof},
			want:    "https://profile",
		},
		{
			name:    "default when nothing set",
			sources: Sources{},
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.sources)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Endpoint)
		})
	}
}

func TestResolveUsernameAndPassword(t *testing.T) {
	env := mapLookup(map[string]string{
		"OPENSEARCH_USERNAME": "envuser",
		"OPENSEARCH_PASSWORD": "envpass",
	})

	t.Run("username and password from env", func(t *testing.T) {
		got, err := Resolve(Sources{Env: env, UseEnv: true})
		require.NoError(t, err)
		assert.Equal(t, "envuser", got.Username)
		assert.Equal(t, "envpass", got.Password)
	})

	t.Run("flag username beats env", func(t *testing.T) {
		got, err := Resolve(Sources{
			Flags:   Config{Username: "flaguser"},
			Changed: changedSet(FieldUsername),
			Env:     env, UseEnv: true,
		})
		require.NoError(t, err)
		assert.Equal(t, "flaguser", got.Username)
		assert.Equal(t, "envpass", got.Password)
	})

	t.Run("flag password beats env", func(t *testing.T) {
		got, err := Resolve(Sources{
			Flags:   Config{Password: "flagpass"},
			Changed: changedSet(FieldPassword),
			Env:     env, UseEnv: true,
		})
		require.NoError(t, err)
		assert.Equal(t, "flagpass", got.Password)
	})

	t.Run("password never from profile", func(t *testing.T) {
		got, err := Resolve(Sources{Profile: &Profile{Username: "pu"}})
		require.NoError(t, err)
		assert.Equal(t, "pu", got.Username)
		assert.Empty(t, got.Password)
	})

	t.Run("env password ignored when UseEnv false", func(t *testing.T) {
		got, err := Resolve(Sources{
			Profile: &Profile{Username: "envuser"},
			Env:     env, UseEnv: false,
		})
		require.NoError(t, err)
		assert.Equal(t, "envuser", got.Username)
		assert.Empty(t, got.Password)
	})
}

func TestResolveEmptyEnvTreatedAsUnset(t *testing.T) {
	prof := &Profile{Endpoint: "https://profile", Username: "profuser"}
	env := mapLookup(map[string]string{
		"OPENSEARCH_URL":      "",
		"OPENSEARCH_USERNAME": "",
		"OPENSEARCH_PASSWORD": "",
	})

	got, err := Resolve(Sources{Env: env, UseEnv: true, Profile: prof})
	require.NoError(t, err)
	assert.Equal(t, "https://profile", got.Endpoint)
	assert.Equal(t, "profuser", got.Username)
	assert.Empty(t, got.Password)
}

func TestResolveClonesSlices(t *testing.T) {
	src := []int{503}
	prof := &Profile{Retry: &RetryDefaults{RetryStatus: src}}

	got, err := Resolve(Sources{Profile: prof})
	require.NoError(t, err)
	require.Equal(t, []int{503}, got.Retry.RetryStatus)

	got.Retry.RetryStatus[0] = 999
	assert.Equal(t, 503, src[0], "resolved slice must not alias the source")
}

func TestResolveInsecureAndCACert(t *testing.T) {
	prof := &Profile{Insecure: true, CACertPath: "/profile/ca.pem"}

	got, err := Resolve(Sources{Profile: prof})
	require.NoError(t, err)
	assert.True(t, got.Insecure)
	assert.Equal(t, "/profile/ca.pem", got.CACertPath)

	got, err = Resolve(Sources{
		Flags:   Config{Insecure: false, CACertPath: "/flag/ca.pem"},
		Changed: changedSet(FieldInsecure, FieldCACert),
		Profile: prof,
	})
	require.NoError(t, err)
	assert.False(t, got.Insecure)
	assert.Equal(t, "/flag/ca.pem", got.CACertPath)
}

func ptr[T any](v T) *T { return &v }

func TestResolveRetryProfilePointerSemantics(t *testing.T) {
	t.Run("set fields override, unset keep default", func(t *testing.T) {
		prof := &Profile{Retry: &RetryDefaults{
			MaxAttempts: ptr(7),
			Strategy:    ptr("exponential"),
			// Initial/Max unset => keep defaults
			RetryStatus: []int{503},
		}}
		got, err := Resolve(Sources{Profile: prof})
		require.NoError(t, err)
		assert.Equal(t, 7, got.Retry.MaxAttempts)
		assert.Equal(t, Exponential, got.Retry.Strategy)
		assert.Equal(t, 2*time.Second, got.Retry.Initial) // default kept
		assert.Equal(t, 30*time.Second, got.Retry.Max)    // default kept
		assert.Equal(t, []int{503}, got.Retry.RetryStatus)
	})

	t.Run("duration strings parsed", func(t *testing.T) {
		prof := &Profile{Retry: &RetryDefaults{
			Initial: ptr("5s"),
			Max:     ptr("1m"),
			Jitter:  ptr(0.25),
		}}
		got, err := Resolve(Sources{Profile: prof})
		require.NoError(t, err)
		assert.Equal(t, 5*time.Second, got.Retry.Initial)
		assert.Equal(t, time.Minute, got.Retry.Max)
		assert.InDelta(t, 0.25, got.Retry.Jitter, 1e-9)
	})

	t.Run("bad strategy errors", func(t *testing.T) {
		prof := &Profile{Retry: &RetryDefaults{Strategy: ptr("nope")}}
		_, err := Resolve(Sources{Profile: prof})
		require.Error(t, err)
	})

	t.Run("bad duration errors", func(t *testing.T) {
		prof := &Profile{Retry: &RetryDefaults{Initial: ptr("abc")}}
		_, err := Resolve(Sources{Profile: prof})
		require.Error(t, err)
	})
}

func TestResolveRetryFlagsBeatProfile(t *testing.T) {
	prof := &Profile{Retry: &RetryDefaults{MaxAttempts: ptr(7), TerminalStatus: []int{409, 404}}}
	flags := Defaults()
	flags.Retry.MaxAttempts = 3
	flags.Retry.Strategy = Constant
	flags.Retry.Initial = 500 * time.Millisecond
	flags.Retry.Max = 10 * time.Second
	flags.Retry.Jitter = 0.5
	flags.Retry.TerminalStatus = []int{500}
	flags.Retry.RetryStatus = []int{503}
	flags.Retry.SuccessStatus = []int{201}
	flags.Retry.ExpectEmpty = true

	got, err := Resolve(Sources{
		Flags: flags,
		Changed: changedSet(FieldMaxAttempts, FieldBackoff, FieldBackoffInitial,
			FieldBackoffMax, FieldBackoffJitter, FieldTerminalStatus,
			FieldRetryStatus, FieldSuccessStatus, FieldExpectEmpty),
		Profile: prof,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, got.Retry.MaxAttempts)
	assert.Equal(t, Constant, got.Retry.Strategy)
	assert.Equal(t, 500*time.Millisecond, got.Retry.Initial)
	assert.Equal(t, 10*time.Second, got.Retry.Max)
	assert.InDelta(t, 0.5, got.Retry.Jitter, 1e-9)
	assert.Equal(t, []int{500}, got.Retry.TerminalStatus)
	assert.Equal(t, []int{503}, got.Retry.RetryStatus)
	assert.Equal(t, []int{201}, got.Retry.SuccessStatus)
	assert.True(t, got.Retry.ExpectEmpty)
}
