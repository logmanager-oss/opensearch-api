package config

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigPasswordRedaction(t *testing.T) {
	cfg := Config{Endpoint: "https://os:9200", Username: "admin", Password: "s3cret"}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, cfg)
		assert.NotContains(t, out, "s3cret", "verb %s leaked password", verb)
		assert.Contains(t, out, "***", "verb %s missing redaction", verb)
		assert.Contains(t, out, "https://os:9200", "verb %s dropped endpoint", verb)
		assert.Contains(t, out, "admin", "verb %s dropped username", verb)
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

	assert.Equal(t, 0, d.Retry.MaxRetries)
	assert.Equal(t, Linear, d.Retry.Strategy)
	assert.Equal(t, 2*time.Second, d.Retry.Initial)
	assert.Equal(t, 30*time.Second, d.Retry.Max)
	assert.Zero(t, d.Retry.Jitter)
	assert.Nil(t, d.Retry.AbortOn)
	assert.Empty(t, d.Retry.RetryWhen)
	assert.Empty(t, d.Retry.SuccessWhen)
	assert.Equal(t, int64(10*1024*1024), d.Retry.MaxBodyBuffer)

	// The flag-default string and the parsed struct default must never drift.
	parsed, err := ParseSize(DefaultMaxBodyBuffer)
	require.NoError(t, err)
	assert.Equal(t, parsed, d.Retry.MaxBodyBuffer)
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "bare bytes", input: "512", want: 512},
		{name: "zero", input: "0", want: 0},
		{name: "explicit bytes unit", input: "512B", want: 512},
		{name: "KiB", input: "10KiB", want: 10 * 1024},
		{name: "MiB", input: "10MiB", want: 10 * 1024 * 1024},
		{name: "GiB", input: "2GiB", want: 2 * 1024 * 1024 * 1024},
		{name: "KB alias is 1024-based", input: "10KB", want: 10 * 1024},
		{name: "MB alias is 1024-based", input: "10MB", want: 10 * 1024 * 1024},
		{name: "GB alias is 1024-based", input: "2GB", want: 2 * 1024 * 1024 * 1024},
		{name: "lowercase unit", input: "10mib", want: 10 * 1024 * 1024},
		{name: "mixed case unit", input: "10MiB", want: 10 * 1024 * 1024},
		{name: "empty", input: "", wantErr: true},
		{name: "decimal", input: "1.5MiB", wantErr: true},
		{name: "negative", input: "-1MiB", wantErr: true},
		{name: "unknown unit", input: "10TiB", wantErr: true},
		{name: "unit with no number", input: "MiB", wantErr: true},
		{name: "unit multiplication overflows int64", input: "8589934592GiB", wantErr: true},
		{name: "digits overflow int64", input: "99999999999999999999", wantErr: true},
		{name: "default constant round-trips", input: DefaultMaxBodyBuffer, want: 10 * 1024 * 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSize(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), fmt.Sprintf("%q", tt.input))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
