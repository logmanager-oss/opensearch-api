package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/logmanager-oss/opensearch-api/internal/cli"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"canceled", context.Canceled, 130},
		{"timeout", fmt.Errorf("x: %w", cli.ErrTimeout), 124},
		{"other", errors.New("other"), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, exitCode(tt.err))
		})
	}
}
