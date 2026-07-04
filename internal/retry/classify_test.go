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
			name:   "default 409 not terminal without terminal list",
			cfg:    config.RetryConfig{},
			status: 409,
			want:   Retry,
		},
		{
			name:   "custom terminal status",
			cfg:    config.RetryConfig{TerminalStatus: []int{409}},
			status: 409,
			want:   Terminal,
		},
		{
			name:   "success status list, listed non-2xx succeeds",
			cfg:    config.RetryConfig{SuccessStatus: []int{404}},
			status: 404,
			want:   Success,
		},
		{
			name:   "success status list, unlisted 2xx not success -> retry",
			cfg:    config.RetryConfig{SuccessStatus: []int{201}},
			status: 200,
			want:   Retry,
		},
		{
			name:   "404 in terminal status is terminal",
			cfg:    config.RetryConfig{TerminalStatus: []int{409, 404}},
			status: 404,
			want:   Terminal,
		},
		{
			name:   "404 retried by default",
			cfg:    config.RetryConfig{},
			status: 404,
			want:   Retry,
		},
		{
			name:   "retry whitelist: listed code retries",
			cfg:    config.RetryConfig{RetryStatus: []int{503}},
			status: 503,
			want:   Retry,
		},
		{
			name:   "retry whitelist: unlisted non-success is terminal",
			cfg:    config.RetryConfig{RetryStatus: []int{503}},
			status: 500,
			want:   Terminal,
		},
		{
			name:   "overlap terminal wins over retry whitelist",
			cfg:    config.RetryConfig{TerminalStatus: []int{503}, RetryStatus: []int{503}},
			status: 503,
			want:   Terminal,
		},
		{
			name:   "success beats terminal on overlap",
			cfg:    config.RetryConfig{TerminalStatus: []int{200}},
			status: 200,
			want:   Success,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.cfg, tt.status, tt.transportErr)
			assert.Equal(t, tt.want, got, "classify(%d)", tt.status)
		})
	}
}
