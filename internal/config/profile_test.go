package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadProfilesMissingFile(t *testing.T) {
	pf, err := LoadProfiles(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)
	assert.Empty(t, pf.Profiles)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.yaml")

	maxAtt := 5
	strategyName := "exponential"
	pf := ProfileFile{Profiles: []Profile{
		{
			Name:       "prod",
			Endpoint:   "https://os:9200",
			Username:   "svc-loader",
			CACertPath: "/etc/ca.pem",
			Insecure:   true,
			Retry: &RetryDefaults{
				MaxAttempts:    &maxAtt,
				Strategy:       &strategyName,
				TerminalStatus: []int{409, 404},
			},
		},
	}}

	require.NoError(t, SaveProfiles(path, pf))

	loaded, err := LoadProfiles(path)
	require.NoError(t, err)
	assert.Equal(t, pf, loaded)
}

func TestSaveProfilesPerms(t *testing.T) {
	assertTightPerms := func(t *testing.T, path string) {
		t.Helper()
		fi, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

		di, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), di.Mode().Perm())
	}

	t.Run("creates with tight perms", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cfg", "config.yaml")
		require.NoError(t, SaveProfiles(path, ProfileFile{}))
		assertTightPerms(t, path)
	})

	t.Run("tightens pre-existing loose perms", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "cfg")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("profiles: []\n"), 0o644))

		require.NoError(t, SaveProfiles(path, ProfileFile{}))
		assertTightPerms(t, path)
	})
}

func TestSaveProfilesNoPasswordKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	pf := ProfileFile{Profiles: []Profile{{Name: "p", Username: "u"}}}
	require.NoError(t, SaveProfiles(path, pf))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, strings.ToLower(string(data)), "password")

	// Guard against a future password field slipping in (empty+omitempty
	// would emit no key, so the on-disk check alone is not enough).
	for _, typ := range []reflect.Type{reflect.TypeOf(Profile{}), reflect.TypeOf(RetryDefaults{})} {
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			assert.NotContains(t, name, "password", "%s.%s must not exist", typ.Name(), typ.Field(i).Name)
		}
	}
}

func TestFindProfile(t *testing.T) {
	pf := ProfileFile{Profiles: []Profile{{Name: "a"}, {Name: "b"}}}

	p, ok := FindProfile(pf, "b")
	require.True(t, ok)
	assert.Equal(t, "b", p.Name)

	_, ok = FindProfile(pf, "missing")
	assert.False(t, ok)
}

func TestCheckPerms(t *testing.T) {
	dir := t.TempDir()

	strict := filepath.Join(dir, "strict.yaml")
	require.NoError(t, os.WriteFile(strict, []byte("x"), 0o600))
	assert.NoError(t, CheckPerms(strict))

	loose := filepath.Join(dir, "loose.yaml")
	require.NoError(t, os.WriteFile(loose, []byte("x"), 0o644))
	err := CheckPerms(loose)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLoosePerms)
}

func TestConfigPath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	def := filepath.Join(home, ".osapi", "config.yaml")

	tests := []struct {
		name    string
		flagVal string
		env     map[string]string
		want    string
	}{
		{name: "flag wins", flagVal: "/flag/cfg.yaml", env: map[string]string{"OSAPI_CONFIG": "/env/cfg.yaml"}, want: "/flag/cfg.yaml"},
		{name: "env next", env: map[string]string{"OSAPI_CONFIG": "/env/cfg.yaml"}, want: "/env/cfg.yaml"},
		{name: "default", want: def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConfigPath(tt.flagVal, mapLookup(tt.env))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConfigPathNilEnv(t *testing.T) {
	got, err := ConfigPath("/x.yaml", nil)
	require.NoError(t, err)
	assert.Equal(t, "/x.yaml", got)
}
