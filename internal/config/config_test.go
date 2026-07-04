package config

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigPasswordRedaction(t *testing.T) {
	cfg := Config{Endpoint: "https://os:9200", Username: "boot", Password: "s3cret"}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, cfg)
		assert.NotContains(t, out, "s3cret", "verb %s leaked password", verb)
		assert.Contains(t, out, "***", "verb %s missing redaction", verb)
		assert.Contains(t, out, "https://os:9200", "verb %s dropped endpoint", verb)
		assert.Contains(t, out, "boot", "verb %s dropped username", verb)
	}

	empty := Config{Endpoint: "https://os:9200"}
	assert.NotContains(t, empty.String(), redacted)
	assert.NotContains(t, empty.GoString(), redacted)
}

func TestParseBackoffStrategy(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    BackoffStrategy
		wantErr bool
	}{
		{name: "constant", input: "constant", want: Constant},
		{name: "linear", input: "linear", want: Linear},
		{name: "exponential", input: "exponential", want: Exponential},
		{name: "mixed case", input: "Linear", want: Linear},
		{name: "upper case", input: "EXPONENTIAL", want: Exponential},
		{name: "unknown", input: "fibonacci", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBackoffStrategy(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBackoffStrategyString(t *testing.T) {
	assert.Equal(t, "constant", Constant.String())
	assert.Equal(t, "linear", Linear.String())
	assert.Equal(t, "exponential", Exponential.String())

	for _, s := range []BackoffStrategy{Constant, Linear, Exponential} {
		got, err := ParseBackoffStrategy(s.String())
		require.NoError(t, err)
		assert.Equal(t, s, got)
	}
}

func TestDefaults(t *testing.T) {
	d := Defaults()

	assert.Empty(t, d.Endpoint)
	assert.Empty(t, d.Username)
	assert.Empty(t, d.Password)
	assert.False(t, d.Insecure)

	assert.Equal(t, 0, d.Retry.MaxAttempts)
	assert.Equal(t, Linear, d.Retry.Strategy)
	assert.Equal(t, 2*time.Second, d.Retry.Initial)
	assert.Equal(t, 30*time.Second, d.Retry.Max)
	assert.Zero(t, d.Retry.Jitter)
	assert.Equal(t, []int{409}, d.Retry.TerminalStatus)
	assert.Nil(t, d.Retry.SuccessStatus)
	assert.Nil(t, d.Retry.RetryStatus)
	assert.False(t, d.Retry.ExpectEmpty)
}
