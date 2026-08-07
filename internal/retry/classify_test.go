package retry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

func TestOutcomeString(t *testing.T) {
	assert.Equal(t, "Success", Success.String())
	assert.Equal(t, "Retry", Retry.String())
	assert.Equal(t, "Terminal", Terminal.String())
	assert.Equal(t, "Outcome(99)", Outcome(99).String())
}

// newTestEngine builds an Engine with cfg.RetryWhen/SuccessWhen compiled.
//
//nolint:gocritic // hugeParam: RetryConfig passed by value to match Engine's own convention.
func newTestEngine(t *testing.T, cfg config.RetryConfig, opts ...Option) *Engine {
	t.Helper()
	retryWhen, err := CompilePredicate(cfg.RetryWhen)
	require.NoError(t, err)
	successWhen, err := CompilePredicate(cfg.SuccessWhen)
	require.NoError(t, err)
	allOpts := append([]Option{WithRetryWhen(retryWhen), WithSuccessWhen(successWhen)}, opts...)
	return mustNew(t, cfg, allOpts...)
}

func TestClassify(t *testing.T) {
	errTransport := errors.New("connection refused")

	tests := []struct {
		name         string
		cfg          config.RetryConfig
		status       int
		body         []byte
		overflowed   bool
		transportErr error
		successCheck func(context.Context) (bool, error)
		want         Outcome
		wantReason   string
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
		{
			name:       "retry-when truthy retries",
			cfg:        config.RetryConfig{RetryWhen: ".retry"},
			status:     200,
			body:       []byte(`{"retry":true}`),
			want:       Retry,
			wantReason: "--retry-when matched",
		},
		{
			name:   "retry-when falsy falls back to status",
			cfg:    config.RetryConfig{RetryWhen: ".retry"},
			status: 200,
			body:   []byte(`{"retry":false}`),
			want:   Success,
		},
		{
			name:   "retry-when falsy on non-2xx falls back to retry",
			cfg:    config.RetryConfig{RetryWhen: ".retry"},
			status: 503,
			body:   []byte(`{"retry":false}`),
			want:   Retry,
		},
		{
			name:   "success-when truthy succeeds on non-2xx",
			cfg:    config.RetryConfig{SuccessWhen: ".ok"},
			status: 503,
			body:   []byte(`{"ok":true}`),
			want:   Success,
		},
		{
			name:       "success-when falsy retries even on 2xx",
			cfg:        config.RetryConfig{SuccessWhen: ".ok"},
			status:     200,
			body:       []byte(`{"ok":false}`),
			want:       Retry,
			wantReason: "--success-when not satisfied",
		},
		{
			name:       "both set: retry-when wins over success-when",
			cfg:        config.RetryConfig{RetryWhen: ".retry", SuccessWhen: ".ok"},
			status:     200,
			body:       []byte(`{"retry":true,"ok":true}`),
			want:       Retry,
			wantReason: "--retry-when matched",
		},
		{
			name:   "abort-on wins over truthy retry-when",
			cfg:    config.RetryConfig{AbortOn: []int{409}, RetryWhen: "true"},
			status: 409,
			body:   []byte(`{}`),
			want:   Terminal,
		},
		{
			name:       "non-JSON body treated as not satisfied",
			cfg:        config.RetryConfig{SuccessWhen: ".ok"},
			status:     200,
			body:       []byte("not json"),
			want:       Retry,
			wantReason: "--success-when not satisfied",
		},
		{
			name:       "empty body treated as not satisfied",
			cfg:        config.RetryConfig{SuccessWhen: ".ok"},
			status:     200,
			want:       Retry,
			wantReason: "--success-when not satisfied",
		},
		{
			name:       "overflowed body treated as not satisfied",
			cfg:        config.RetryConfig{SuccessWhen: ".ok"},
			status:     200,
			body:       []byte(`{"ok":true}`),
			overflowed: true,
			want:       Retry,
			wantReason: "--success-when not satisfied",
		},
		{
			name:         "success check passes on 2xx",
			status:       200,
			successCheck: func(context.Context) (bool, error) { return true, nil },
			want:         Success,
		},
		{
			name:         "success check fails on 2xx",
			status:       200,
			successCheck: func(context.Context) (bool, error) { return false, nil },
			want:         Retry,
			wantReason:   "success check failed",
		},
		{
			name:         "success check still consulted when retry-when is falsy",
			cfg:          config.RetryConfig{RetryWhen: ".retry"},
			status:       200,
			body:         []byte(`{"retry":false}`),
			successCheck: func(context.Context) (bool, error) { return false, nil },
			want:         Retry,
			wantReason:   "success check failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEngine(t, tt.cfg, WithSuccessCheck(tt.successCheck))
			got, reason, err := e.classify(context.Background(), tt.status, tt.body, tt.overflowed, tt.transportErr)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got, "classify(%d)", tt.status)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestClassifyEmptyBodyWarningText(t *testing.T) {
	var buf bytes.Buffer
	e := newTestEngine(t, config.RetryConfig{SuccessWhen: ".ok"}, WithWarn(&buf))

	_, _, _ = e.classify(context.Background(), 200, nil, false, nil)
	assert.Equal(t, "empty response body; --retry-when/--success-when not evaluated\n", buf.String())
}

func TestClassifyOverflowWarningText(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.RetryConfig{SuccessWhen: ".ok", MaxBodyBuffer: 10 * 1024 * 1024}
	e := newTestEngine(t, cfg, WithWarn(&buf))

	_, _, _ = e.classify(context.Background(), 200, nil, true, nil)
	assert.Equal(t, "response body exceeds --max-body-buffer (10MiB); --retry-when/--success-when not evaluated\n", buf.String())
}

func TestClassifyNonJSONBodyWarns(t *testing.T) {
	var buf bytes.Buffer
	e := newTestEngine(t, config.RetryConfig{SuccessWhen: ".ok"}, WithWarn(&buf))

	_, _, _ = e.classify(context.Background(), 200, []byte("not json"), false, nil)
	assert.NotEmpty(t, buf.String())
}

func TestClassifyPredicateEvalErrorWarnsAndNotSatisfied(t *testing.T) {
	var buf bytes.Buffer
	e := newTestEngine(t, config.RetryConfig{SuccessWhen: `error("boom")`}, WithWarn(&buf))

	got, reason, err := e.classify(context.Background(), 200, []byte(`{}`), false, nil)
	require.NoError(t, err)
	assert.Equal(t, Retry, got)
	assert.Equal(t, "--success-when not satisfied", reason)
	assert.NotEmpty(t, buf.String())
}

func TestClassifyContextErrorPropagatesWithoutWarning(t *testing.T) {
	var buf bytes.Buffer
	e := newTestEngine(t, config.RetryConfig{RetryWhen: "repeat(null)"}, WithWarn(&buf))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, reason, err := e.classify(ctx, 200, []byte(`{}`), false, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, reason)
	assert.Empty(t, buf.String(), "ctx errors during predicate evaluation must not warn")
}

func TestClassifyNoPredicatesConfigured(t *testing.T) {
	e := newTestEngine(t, config.RetryConfig{})
	got, reason, err := e.classify(context.Background(), 503, []byte(`{"ok":true}`), false, nil)
	require.NoError(t, err)
	assert.Equal(t, Retry, got)
	assert.Empty(t, reason)
}

func TestClassifySuccessCheckNotInvokedOnNon2xx(t *testing.T) {
	var calls int
	check := func(context.Context) (bool, error) {
		calls++
		return true, nil
	}
	e := newTestEngine(t, config.RetryConfig{}, WithSuccessCheck(check))

	got, reason, err := e.classify(context.Background(), 503, nil, false, nil)
	require.NoError(t, err)
	assert.Equal(t, Retry, got)
	assert.Empty(t, reason)
	assert.Zero(t, calls, "success check must not run on a non-2xx response")
}

func TestClassifySuccessCheckNotInvokedWhenAbortOnWins(t *testing.T) {
	var calls int
	check := func(context.Context) (bool, error) {
		calls++
		return true, nil
	}
	cfg := config.RetryConfig{AbortOn: []int{409}}
	e := newTestEngine(t, cfg, WithSuccessCheck(check))

	got, _, err := e.classify(context.Background(), 409, nil, false, nil)
	require.NoError(t, err)
	assert.Equal(t, Terminal, got)
	assert.Zero(t, calls, "success check must not run when abort-on wins")
}

func TestClassifySuccessCheckNotInvokedWhenRetryWhenMatches(t *testing.T) {
	var calls int
	check := func(context.Context) (bool, error) {
		calls++
		return true, nil
	}
	cfg := config.RetryConfig{RetryWhen: "true"}
	e := newTestEngine(t, cfg, WithSuccessCheck(check))

	got, reason, err := e.classify(context.Background(), 200, []byte(`{}`), false, nil)
	require.NoError(t, err)
	assert.Equal(t, Retry, got)
	assert.Equal(t, "--retry-when matched", reason)
	assert.Zero(t, calls, "success check must not run when --retry-when matches")
}

func TestClassifySuccessCheckErrorPropagates(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		wantIs error
	}{
		{
			name:   "wrapped terminal status",
			err:    fmt.Errorf("nested verify: %w", ErrTerminalStatus),
			wantIs: ErrTerminalStatus,
		},
		{
			name:   "context canceled",
			err:    context.Canceled,
			wantIs: context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := func(context.Context) (bool, error) { return false, tt.err }
			e := newTestEngine(t, config.RetryConfig{}, WithSuccessCheck(check))

			_, _, err := e.classify(context.Background(), 200, nil, false, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantIs)
		})
	}
}
