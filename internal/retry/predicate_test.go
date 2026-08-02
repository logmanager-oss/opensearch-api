package retry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompilePredicate(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantNil bool
		wantErr bool
	}{
		{name: "empty expr means not configured", expr: "", wantNil: true},
		{name: "valid expr compiles", expr: ".status"},
		{name: "invalid expr fails to parse", expr: "(", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := CompilePredicate(tt.expr)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, p)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, p)
				return
			}
			require.NotNil(t, p)
			assert.Equal(t, tt.expr, p.String())
		})
	}
}

func TestPredicateMatch(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		input   any
		want    bool
		wantErr bool
	}{
		{name: "true is truthy", expr: "true", want: true},
		{name: "false is falsy", expr: "false", want: false},
		{name: "null is falsy", expr: "null", want: false},
		{name: "zero is truthy", expr: "0", want: true},
		{name: "empty string is truthy", expr: `""`, want: true},
		{name: "empty array is truthy", expr: "[]", want: true},
		{name: "empty object is truthy", expr: "{}", want: true},
		{name: "field access truthy", expr: ".ok", input: map[string]any{"ok": true}, want: true},
		{name: "field access falsy", expr: ".ok", input: map[string]any{"ok": false}, want: false},
		{name: "multi-output any truthy", expr: ".[]", input: []any{false, nil, true}, want: true},
		{name: "multi-output all falsy", expr: ".[]", input: []any{false, nil}, want: false},
		{name: "eval error propagates", expr: `error("boom")`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := CompilePredicate(tt.expr)
			require.NoError(t, err)
			require.NotNil(t, p)

			got, err := p.match(context.Background(), tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPredicateMatchContextCancelled(t *testing.T) {
	p, err := CompilePredicate("repeat(null)")
	require.NoError(t, err)
	require.NotNil(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = p.match(ctx, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
