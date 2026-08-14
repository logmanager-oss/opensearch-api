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
// capture-failure classes; the other three (empty body, not valid JSON, over
// max-body-buffer) are properties of the response body shared by every
// capture on the call, so decodeCaptureBody reports them directly rather than
// via a sentinel.
var (
	errCaptureMatchedNothing = errors.New("matched nothing")
	errCaptureNotScalar      = errors.New("not a scalar")
)

// run evaluates c's compiled expression against input, taking the first
// emitted value. An emitted error (including a context error surfaced by
// RunWithContext) propagates unwrapped, so cancellation still maps to exit
// code 130 instead of being reported as a capture failure.
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
// Strings render verbatim and bool as true/false. int is gojq's type for a
// literal in the expression itself, but also for length, utf8bytelength,
// tonumber and any integer arithmetic, so this branch is load-bearing, not
// just a literal-parsing artifact. float64 is gojq's type for a JSON number
// an expression has done arithmetic on; strconv.FormatFloat renders it so it
// never grows a trailing ".0". json.Number is what a value reaches this
// function as when an expression (e.g. a bare field access) passes a JSON
// number through untouched: decodeCaptureBody decodes with UseNumber()
// precisely so an integer beyond float64's 2^53 exact range keeps its
// digits, so a json.Number already in integer form renders verbatim, and
// only a fractional/exponential one is routed through the same float64
// formatting — a captured 3.0 still renders "3", not the literal token
// "3.0". *big.Int is gojq's own type for integer arithmetic beyond int64 and
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

// decodeCaptureBody decodes data as JSON for capture evaluation, or reports
// which of the three body-level failure classes applies. UseNumber keeps
// every JSON number as a json.Number instead of collapsing it through
// float64, which silently corrupts any integer beyond 2^53 (renderScalar/
// renderJSONNumber is where that value is rendered back to a string). Unlike
// retry/classify.go's decodeBody, which warns and treats the same conditions
// as "not satisfied" so the engine keeps retrying, a capture failure here is
// returned to the caller, which fails the call immediately.
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

// extractCaptures evaluates captures in document order against data, the
// call's already-read response body, storing each rendered value in store.
// It stops at the first unextractable capture — the "immediate failure" rule
// — naming that capture, its expression, and the failure class. A
// body-level failure (decodeCaptureBody) is reported against captures[0],
// the capture that would have run first: no capture on the call is more or
// less responsible for it than another.
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
// "${" and does not count as a reference; a lone "$" is literal. An
// unterminated "${" (no closing "}") is a malformed reference and stops the
// scan with an error rather than being passed through as a literal "${" —
// otherwise a runbook could load with an unresolved "${" that ships to the
// server untouched. Both load-time reference collection (checkRefs) and
// runtime substitution (substitute/substitutePath) call this, so the two
// can never disagree about what is a reference.
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

// substitute replaces every ${name} in s with store[name]. Load already
// guarantees every reference resolves and every "${" is terminated
// (checkRefs), so scanTemplate cannot fail here in practice; the error is
// dropped rather than propagated because query/header/body substitution
// never fails a call over a captured value's contents — only substitutePath
// does.
func substitute(s string, store map[string]string) string {
	out, _ := scanTemplate(s, func(name string) (string, error) {
		return store[name], nil
	})
	return out
}

// substitutePath resolves ${name} references in a call's path against
// store, additionally rejecting a resolved value containing "/", "?", "#" or
// "%": each would let a captured value repoint the request at a different
// endpoint or inject query parameters, and path is the only field where a
// substituted value changes what is requested rather than merely what is
// sent. The value is not known until the call runs, so this is a runtime
// failure of the call, not something Load can catch.
//
// A per-value check alone is composable around: neither "." nor ".." need
// appear in any single captured value for the resolved path to still gain a
// "." or ".." segment — e.g. path "/next/.${id}" with a captured id of "."
// resolves to ".." even though the value itself is clean. So once a
// substitution has actually happened, the resolved path is re-split on "/"
// and checked for a "." or ".." segment, which a normalizing proxy
// (nginx/Envoy) would otherwise collapse into a parent-directory hop. That
// check runs only when substituted is true: an author's own literal path
// (e.g. containing a real ".." for whatever reason) never went through a
// capture and is trusted as written, exactly like every other field.
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
