package cli

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

func TestVerboseHookDisabled(t *testing.T) {
	assert.Nil(t, verboseHook(io.Discard, false))
}

func TestVerboseHook(t *testing.T) {
	tests := []struct {
		name string
		info retry.RetryInfo
		want string
	}{
		{
			name: "transport error",
			info: retry.RetryInfo{Attempt: 1, Err: errors.New("boom"), Delay: time.Second},
			want: "attempt 1 failed: boom; retrying in 1s\n",
		},
		{
			name: "status without reason",
			info: retry.RetryInfo{Attempt: 2, Status: 503, Delay: 2 * time.Second},
			want: "attempt 2: status 503; retrying in 2s\n",
		},
		{
			name: "status with reason",
			info: retry.RetryInfo{Attempt: 3, Status: 200, Delay: 3 * time.Second, Reason: "--retry-when matched"},
			want: "attempt 3: status 200 (--retry-when matched); retrying in 3s\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			hook := verboseHook(&buf, true)
			hook(tt.info)
			assert.Equal(t, tt.want, buf.String())
		})
	}
}
