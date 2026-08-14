package runbook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

// compileCapture parses and compiles a capture's jq expression, returning
// the Capture ready to run.
func compileCapture(name, expr string) (Capture, error) {
	query, err := gojq.Parse(expr)
	if err != nil {
		return Capture{}, fmt.Errorf("parsing %q: %w", expr, err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return Capture{}, fmt.Errorf("compiling %q: %w", expr, err)
	}
	return Capture{Name: name, Expr: expr, code: code}, nil
}

// errCaptureMatchedNothing and errCaptureNotScalar name two of the five
// capture-failure classes. The other three — empty body, invalid JSON, over
// max-body-buffer — are body-level, so decodeCaptureBody reports them
// directly instead of via a sentinel.
var (
	errCaptureMatchedNothing = errors.New("matched nothing")
	errCaptureNotScalar      = errors.New("not a scalar")
)

// run evaluates c's compiled expression against input, returning the first
// emitted value. An emitted error — including a context error from
// RunWithContext — propagates unwrapped, so cancellation still maps to exit
// code 130, not a capture failure.
func (c Capture) run(ctx context.Context, input any) (any, error) {
	iter := c.code.RunWithContext(ctx, input)
	v, ok := iter.Next()
	if !ok {
		return nil, errCaptureMatchedNothing
	}
	if err, ok := v.(error); ok {
		return nil, err
	}
	return v, nil
}

// renderScalar converts a captured jq value to its stored string form.
// int covers not just a literal number but length, utf8bytelength, tonumber
// and any integer arithmetic, so this branch is load-bearing, not just a
// literal-parsing artifact. float64 is a JSON number an expression has done
// arithmetic on; FormatFloat renders it without a trailing ".0". json.Number
// is an untouched JSON number: decodeCaptureBody decodes with UseNumber() so
// an integer beyond float64's 2^53 exact range keeps its digits, so an
// integer-form json.Number renders verbatim and only a fractional/
// exponential one goes through the same float64 formatting (3.0 renders
// "3"). *big.Int is gojq's own type for integer arithmetic beyond int64 and
// renders via its exact decimal string. null, an object, an array, or any
// other type is not a scalar.
func renderScalar(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case bool:
		return strconv.FormatBool(val), nil
	case int:
		return strconv.Itoa(val), nil
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64), nil
	case json.Number:
		return renderJSONNumber(val)
	case *big.Int:
		return val.String(), nil
	default:
		return "", errCaptureNotScalar
	}
}

// renderJSONNumber renders val exactly when it is already an integer
// literal (no '.', 'e' or 'E'), and via renderScalar's float64 formatting
// otherwise. See renderScalar's doc for why the split matters.
func renderJSONNumber(val json.Number) (string, error) {
	if !strings.ContainsAny(val.String(), ".eE") {
		return val.String(), nil
	}
	f, err := val.Float64()
	if err != nil {
		return "", errCaptureNotScalar
	}
	return strconv.FormatFloat(f, 'f', -1, 64), nil
}

// decodeCaptureBody decodes data as JSON, reporting which of the three
// body-level failure classes applies. UseNumber keeps every JSON number as
// json.Number, avoiding float64's silent corruption of integers beyond 2^53.
// Unlike retry/classify.go's decodeBody, which treats the same conditions as
// "not satisfied" and keeps retrying, a failure here fails the call
// immediately.
func decodeCaptureBody(data []byte, truncated bool, maxBuffer int64) (any, error) {
	if truncated {
		return nil, fmt.Errorf("response body exceeds max-body-buffer (%s)", config.FormatSize(maxBuffer))
	}
	if len(data) == 0 {
		return nil, errors.New("empty response body")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var input any
	if err := dec.Decode(&input); err != nil {
		return nil, fmt.Errorf("response body is not valid JSON: %w", err)
	}
	return input, nil
}

// extractCaptures evaluates captures in document order against data — the
// call's already-read response body — storing each rendered value in store.
// It stops at the first unextractable capture, naming it, its expression and
// the failure class. A body-level failure is attributed to captures[0],
// since no capture is more responsible for it than another.
func extractCaptures(ctx context.Context, captures []Capture, data []byte, truncated bool, maxBuffer int64, store map[string]string) error {
	if len(captures) == 0 {
		return nil // nothing to attribute a body-level failure to, and nothing to extract.
	}

	input, err := decodeCaptureBody(data, truncated, maxBuffer)
	if err != nil {
		return fmt.Errorf("capture %q (%s): %w", captures[0].Name, captures[0].Expr, err)
	}

	for _, c := range captures {
		v, err := c.run(ctx, input)
		if err != nil {
			return fmt.Errorf("capture %q (%s): %w", c.Name, c.Expr, err)
		}
		val, err := renderScalar(v)
		if err != nil {
			return fmt.Errorf("capture %q (%s): %w", c.Name, c.Expr, err)
		}
		store[c.Name] = val
	}
	return nil
}

// scanTemplate walks s, invoking resolve for every unescaped ${name}
// reference and writing its return value in place. "$${" emits a literal
// "${" and is not a reference; a lone "$" is literal. An unterminated "${"
// stops the scan with an error instead of passing through as a literal, so a
// runbook can never ship an unresolved "${" to the server. Both load-time
// checking (checkRefs) and runtime substitution (substitute/substitutePath)
// call this, so the two can never disagree about what is a reference.
func scanTemplate(s string, resolve func(name string) (string, error)) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			b.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated %q starting at byte %d", "${", i)
			}
			resolved, err := resolve(s[i+2 : i+2+end])
			if err != nil {
				return "", err
			}
			b.WriteString(resolved)
			i += 2 + end + 1
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String(), nil
}

// substitute replaces every ${name} in s with store[name]. Load's checkRefs
// already guarantees every reference resolves and every "${" terminates, so
// scanTemplate can't fail here in practice. Its error is dropped because
// only substitutePath, not query/header/body substitution, can fail a call
// over a captured value's contents.
func substitute(s string, store map[string]string) string {
	out, _ := scanTemplate(s, func(name string) (string, error) {
		return store[name], nil
	})
	return out
}

// substitutePath resolves ${name} references in a call's path against
// store, rejecting any resolved value containing "/", "?", "#" or "%" — each
// could redirect the request to a different endpoint or inject query
// parameters, and path is the only field where a substituted value changes
// what is requested rather than merely what is sent. This is a runtime
// check, not a load-time one: the value isn't known until the call runs.
//
// A per-value check alone can be walked around: neither "." nor ".." needs
// to appear in a single captured value for the resolved path to still gain a
// ".." segment (e.g. path "/next/.${id}" with id="." resolves to ".."). So
// once a substitution happens, the resolved path is re-split on "/" and
// checked for a "." or ".." segment, which a normalizing proxy (nginx/Envoy)
// would otherwise collapse into a parent-directory hop. This check only runs
// when substitution happened: an author's own literal path is trusted as
// written, like every other field.
func substitutePath(s string, store map[string]string) (string, error) {
	substituted := false
	out, err := scanTemplate(s, func(name string) (string, error) {
		substituted = true
		val := store[name]
		if i := strings.IndexAny(val, `/?#%`); i >= 0 {
			return "", fmt.Errorf("capture ${%s} = %q cannot be substituted into path: contains %q", name, val, string(val[i]))
		}
		return val, nil
	})
	if err != nil || !substituted {
		return out, err
	}
	for seg := range strings.SplitSeq(out, "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("substituted path %q contains a %q segment", out, seg)
		}
	}
	return out, nil
}
