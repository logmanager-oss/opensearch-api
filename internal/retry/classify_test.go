package retry

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

func TestOutcomeString(t *testing.T) {
	assert.Equal(t, "Success", Success.String())
	assert.Equal(t, "Retry", Retry.String())
	assert.Equal(t, "Terminal", Terminal.String())
	assert.Equal(t, "Outcome(99)", Outcome(99).String())
}

func TestClassify(t *testing.T) {
	errTransport := errors.New("connection refused")

	tests := []struct {
		name         string
		cfg          config.RetryConfig
		status       int
		transportErr error
		want         Outcome
	}{
		{
			name:         "transport error retries",
			status:       200,
			transportErr: errTransport,
			want:         Retry,
		},
		{
			name:   "default 2xx success",
			status: 204,
			want:   Success,
		},
		{
			name:   "default non-2xx retries",
			cfg:    config.RetryConfig{},
			status: 503,
			want:   Retry,
		},
		{
			name:   "non-2xx not in abort-on retries",
			cfg:    config.RetryConfig{},
			status: 409,
			want:   Retry,
		},
		{
			name:   "abort-on code is terminal",
			cfg:    config.RetryConfig{AbortOn: []int{409}},
			status: 409,
			want:   Terminal,
		},
		{
			name:   "abort-on with multiple codes",
			cfg:    config.RetryConfig{AbortOn: []int{400, 404, 409}},
			status: 404,
			want:   Terminal,
		},
		{
			name:   "non-abort-on non-2xx still retries",
			cfg:    config.RetryConfig{AbortOn: []int{409}},
			status: 500,
			want:   Retry,
		},
		{
			name:   "2xx is success even if listed in abort-on",
			cfg:    config.RetryConfig{AbortOn: []int{200}},
			status: 200,
			want:   Success,
		},
		{
			name:         "transport error retries even with abort-on set",
			cfg:          config.RetryConfig{AbortOn: []int{409}},
			status:       409,
			transportErr: errTransport,
			want:         Retry,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.cfg, tt.status, tt.transportErr)
			assert.Equal(t, tt.want, got, "classify(%d)", tt.status)
		})
	}
}
