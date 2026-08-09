package runbook

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The full renderScalar number matrix: int/float64 (unchanged by the
// UseNumber fix) and json.Number/*big.Int (the two new cases). null, an
// array and an object must all still be rejected — accepting null as "null"
// is a real mutation that survived the load/run-level suite because no
// runbook capture expression happens to emit a bare null through renderScalar
// without also failing some earlier check.
func TestRenderScalar(t *testing.T) {
	bigBeyondInt64, ok := new(big.Int).SetString("123456789012345678901234567890", 10)
	require.True(t, ok)

	tests := []struct {
		name    string
		v       any
		want    string
		wantErr bool
	}{
		{name: "string", v: "hello", want: "hello"},
		{name: "bool true", v: true, want: "true"},
		{name: "bool false", v: false, want: "false"},
		{name: "int zero", v: 0, want: "0"},
		{name: "int five", v: 5, want: "5"},
		{name: "int negative", v: -42, want: "-42"},
		{name: "float with a fraction", v: 1.5, want: "1.5"},
		{name: "whole float has no trailing .0", v: 3.0, want: "3"},
		{name: "json.Number integer zero", v: json.Number("0"), want: "0"},
		{name: "json.Number integer five", v: json.Number("5"), want: "5"},
		{name: "json.Number integer negative", v: json.Number("-42"), want: "-42"},
		{name: "json.Number with a fraction", v: json.Number("1.5"), want: "1.5"},
		{name: "json.Number whole float loses the literal trailing .0", v: json.Number("3.0"), want: "3"},
		{name: "json.Number scientific notation expands to plain digits", v: json.Number("1e21"), want: "1000000000000000000000"},
		{name: "json.Number small scientific notation expands to plain digits", v: json.Number("1e-7"), want: "0.0000001"},
		{name: "json.Number one beyond float64's exact-integer range stays exact", v: json.Number("9007199254740993"), want: "9007199254740993"},
		{name: "json.Number far beyond int64 stays exact", v: json.Number("1234567890123456789"), want: "1234567890123456789"},
		{name: "*big.Int renders its exact decimal string", v: big.NewInt(123), want: "123"},
		{name: "*big.Int beyond int64 renders exactly", v: bigBeyondInt64, want: "123456789012345678901234567890"},
		{name: "null is not a scalar", v: nil, wantErr: true},
		{name: "array is not a scalar", v: []any{1, 2, 3}, wantErr: true},
		{name: "object is not a scalar", v: map[string]any{"a": 1}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderScalar(tt.v)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, errCaptureNotScalar)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// length is gojq's own int, produced without ever touching decodeCaptureBody's
// json.Number leaves — unlike {"v":42} whose 42 only reaches renderScalar via
// the json.Number branch. Both must render the same way, but only this one
// exercises the literal `case int` branch with a value gojq computed itself.
func TestCaptureRunGenuineIntFromJQArithmetic(t *testing.T) {
	c, err := compileCapture("n", ".hits | length")
	require.NoError(t, err)

	v, err := c.run(context.Background(), map[string]any{"hits": []any{1, 2, 3}})
	require.NoError(t, err)
	assert.IsType(t, int(0), v, "length must yield gojq's native int, not a JSON-decoded number type")

	got, err := renderScalar(v)
	require.NoError(t, err)
	assert.Equal(t, "3", got)
}

func TestDecodeCaptureBody(t *testing.T) {
	t.Run("truncated reports the configured max-body-buffer", func(t *testing.T) {
		_, err := decodeCaptureBody([]byte(`{"v":1}`), true, 1024)
		require.Error(t, err)
		assert.ErrorContains(t, err, "max-body-buffer")
	})

	t.Run("empty body", func(t *testing.T) {
		_, err := decodeCaptureBody(nil, false, 0)
		require.Error(t, err)
		assert.ErrorContains(t, err, "empty")
	})

	t.Run("not valid JSON", func(t *testing.T) {
		_, err := decodeCaptureBody([]byte("not-json"), false, 0)
		require.Error(t, err)
		assert.ErrorContains(t, err, "not valid JSON")
	})

	t.Run("numbers decode as json.Number, not float64", func(t *testing.T) {
		input, err := decodeCaptureBody([]byte(`{"v":9007199254740993}`), false, 0)
		require.NoError(t, err)
		m, ok := input.(map[string]any)
		require.True(t, ok)
		n, ok := m["v"].(json.Number)
		require.True(t, ok, "want json.Number, got %T", m["v"])
		assert.Equal(t, "9007199254740993", n.String())
	})
}

func TestExtractCaptures(t *testing.T) {
	mustCapture := func(t *testing.T, name, expr string) Capture {
		t.Helper()
		c, err := compileCapture(name, expr)
		require.NoError(t, err)
		return c
	}

	t.Run("a failing capture leaves the store untouched", func(t *testing.T) {
		// .v is an object: not a scalar.
		captures := []Capture{mustCapture(t, "v", ".v")}
		store := map[string]string{}

		err := extractCaptures(context.Background(), captures, []byte(`{"v":{"a":1}}`), false, 0, store)
		require.Error(t, err)
		assert.Empty(t, store, "the value must never be stored before the scalar check runs")
	})

	t.Run("the error names the jq expression", func(t *testing.T) {
		captures := []Capture{mustCapture(t, "v", ".v[] | select(. == 99)")}
		store := map[string]string{}

		err := extractCaptures(context.Background(), captures, []byte(`{"v":[1,2,3]}`), false, 0, store)
		require.Error(t, err)
		assert.ErrorContains(t, err, ".v[] | select(. == 99)")
	})

	t.Run("a failing first capture stops the second from running or storing", func(t *testing.T) {
		captures := []Capture{
			mustCapture(t, "a", ".a"), // .a is an object: not a scalar, fails first.
			mustCapture(t, "b", ".b"), // would succeed if it ever ran.
		}
		store := map[string]string{}

		err := extractCaptures(context.Background(), captures, []byte(`{"a":{"x":1},"b":"ok"}`), false, 0, store)
		require.Error(t, err)
		assert.ErrorContains(t, err, `capture "a"`, "the first capture is the one that failed")
		assert.NotContains(t, err.Error(), `capture "b"`, "extraction must stop at the first failure")
		assert.Empty(t, store, "the second capture must not run or store once the first fails")
	})

	t.Run("captures store in document order on success", func(t *testing.T) {
		captures := []Capture{mustCapture(t, "a", ".a"), mustCapture(t, "b", ".b")}
		store := map[string]string{}

		err := extractCaptures(context.Background(), captures, []byte(`{"a":"1","b":"2"}`), false, 0, store)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1", "b": "2"}, store)
	})

	t.Run("a body-level failure is reported against the first capture", func(t *testing.T) {
		captures := []Capture{mustCapture(t, "a", ".a"), mustCapture(t, "b", ".b")}
		store := map[string]string{}

		err := extractCaptures(context.Background(), captures, nil, false, 0, store)
		require.Error(t, err)
		assert.ErrorContains(t, err, `capture "a"`)
		assert.ErrorContains(t, err, "empty")
	})

	// No captures means nothing to attribute a body-level failure to; indexing
	// captures[0] here would panic even though the call has nothing to fail.
	t.Run("no captures on the call never indexes captures[0], even with an invalid body", func(t *testing.T) {
		store := map[string]string{}

		err := extractCaptures(context.Background(), nil, []byte("not-json"), false, 0, store)
		require.NoError(t, err)
		assert.Empty(t, store)
	})
}

func TestScanTemplate(t *testing.T) {
	tests := []struct {
		name      string
		s         string
		wantNames []string
		wantOut   string
	}{
		{name: "an escaped dollar-brace before a real reference", s: "$${a}${b}", wantNames: []string{"b"}, wantOut: "${a}<b>"},
		{name: "a real reference before an escaped dollar-brace", s: "${a}$${b}", wantNames: []string{"a"}, wantOut: "<a>${b}"},
		{name: "an empty reference name", s: "${}", wantNames: []string{""}, wantOut: "<>"},
		{name: "a nested ${ is not special-cased; the first } closes the reference", s: "${${a}}", wantNames: []string{"${a"}, wantOut: "<${a>}"},
		{name: "a second, unterminated ${ inside a name is swallowed by the first }", s: "${a${b}", wantNames: []string{"a${b"}, wantOut: "<a${b>"},
		{name: "whitespace in a name is kept verbatim", s: "${ a }", wantNames: []string{" a "}, wantOut: "< a >"},
		{name: "no ${ at all takes the fast path and never calls resolve", s: "no references here", wantNames: nil, wantOut: "no references here"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var names []string
			out, err := scanTemplate(tt.s, func(name string) (string, error) {
				names = append(names, name)
				return "<" + name + ">", nil
			})
			require.NoError(t, err)
			assert.Equal(t, tt.wantNames, names)
			assert.Equal(t, tt.wantOut, out)
		})
	}
}

// substitutePath must reject both a substituted value that injects a
// different endpoint via "/ ? # %" and one that resolves to a "." or ".."
// path segment, while leaving an author's own literal path untouched even
// when it contains "..", since no substitution ran to make it untrusted.
func TestSubstitutePath(t *testing.T) {
	tests := []struct {
		name       string
		s          string
		store      map[string]string
		want       string
		wantErrIs  string // substring the error must contain, if any
		wantErrNil bool
	}{
		{
			name:  "no references at all: a literal .. segment passes through untouched",
			s:     "/a/../b",
			store: nil,
			want:  "/a/../b",
		},
		{
			name:  "normal value substitutes cleanly",
			s:     "/next/${id}",
			store: map[string]string{"id": "42"},
			want:  "/next/42",
		},
		{
			name:      "substituted value .. is rejected",
			s:         "/next/${id}",
			store:     map[string]string{"id": ".."},
			wantErrIs: `contains a ".." segment`,
		},
		{
			name:      "substituted value . completes a .. segment with a literal .",
			s:         "/next/.${id}",
			store:     map[string]string{"id": "."},
			wantErrIs: `contains a ".." segment`,
		},
		{
			name:      "substituted value containing a slash is still rejected",
			s:         "/next/${id}",
			store:     map[string]string{"id": "a/b"},
			wantErrIs: `contains "/"`,
		},
		{
			name:      "substituted value containing a question mark is still rejected",
			s:         "/next/${id}",
			store:     map[string]string{"id": "a?b"},
			wantErrIs: `contains "?"`,
		},
		{
			name:      "substituted value containing a hash is still rejected",
			s:         "/next/${id}",
			store:     map[string]string{"id": "a#b"},
			wantErrIs: `contains "#"`,
		},
		{
			name:      "substituted value containing a percent is still rejected",
			s:         "/next/${id}",
			store:     map[string]string{"id": "a%b"},
			wantErrIs: `contains "%"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := substitutePath(tt.s, tt.store)
			if tt.wantErrIs != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErrIs)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// An unterminated ${ must stop the scan with an error instead of being
// passed through as a literal — and, since resolve never runs for it, the
// same malformed reference can never reach the server as an unresolved
// "${" even if a caller ignored scanTemplate's error.
func TestScanTemplateUnterminatedReferenceIsAnError(t *testing.T) {
	var called bool
	_, err := scanTemplate(`{"a":"x", "b":${seq`, func(string) (string, error) {
		called = true
		return "", nil
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "unterminated")
	assert.False(t, called, "resolve must never run for a reference that never closes")
}
