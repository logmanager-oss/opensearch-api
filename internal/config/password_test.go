package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvePassword(t *testing.T) {
	noPrompt := func() (string, error) {
		t.Helper()
		t.Fatal("prompt should not be called")
		return "", nil
	}

	t.Run("existing password returned as-is", func(t *testing.T) {
		got, err := ResolvePassword(Config{Username: "u", Password: "p"}, noPrompt, true)
		require.NoError(t, err)
		assert.Equal(t, "p", got)
	})

	t.Run("empty username means no auth, no prompt", func(t *testing.T) {
		got, err := ResolvePassword(Config{}, noPrompt, true)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("tty prompts when username set and no password", func(t *testing.T) {
		called := false
		prompt := func() (string, error) {
			called = true
			return "prompted", nil
		}
		got, err := ResolvePassword(Config{Username: "u"}, prompt, true)
		require.NoError(t, err)
		assert.True(t, called)
		assert.Equal(t, "prompted", got)
	})

	t.Run("prompt error propagated", func(t *testing.T) {
		sentinel := errors.New("boom")
		prompt := func() (string, error) { return "", sentinel }
		_, err := ResolvePassword(Config{Username: "u"}, prompt, true)
		require.ErrorIs(t, err, sentinel)
	})

	t.Run("non-tty with no password errors", func(t *testing.T) {
		_, err := ResolvePassword(Config{Username: "u"}, noPrompt, false)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNoPassword)
	})

	t.Run("nil prompt on tty errors instead of panicking", func(t *testing.T) {
		_, err := ResolvePassword(Config{Username: "u"}, nil, true)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNoPassword)
	})
}
