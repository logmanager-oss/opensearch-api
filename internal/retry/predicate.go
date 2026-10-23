package retry

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/itchyny/gojq"
)

// Predicate is a compiled jq expression evaluated against a decoded JSON
// response body. A nil *Predicate means "not configured".
type Predicate struct {
	expr string
	code *gojq.Code
}

// CompilePredicate compiles a jq expression for retry/success classification.
// An empty expr means "not configured" and returns (nil, nil).
func CompilePredicate(expr string) (*Predicate, error) {
	if expr == "" {
		return nil, nil
	}
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("parsing predicate %q: %w", expr, err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, fmt.Errorf("compiling predicate %q: %w", expr, err)
	}
	return &Predicate{expr: expr, code: code}, nil
}

// String returns the original jq expression, for warning messages.
func (p *Predicate) String() string {
	return p.expr
}

// Match reports whether p is truthy against the JSON document body.
// A body that is not valid JSON is an error.
func (p *Predicate) Match(ctx context.Context, body []byte) (bool, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, fmt.Errorf("decoding body for predicate %q: %w", p.expr, err)
	}
	return p.match(ctx, doc)
}

// match reports jq truthiness (any emitted value that is neither null nor
// false) of running p against input. An emitted error value, including a
// context error surfaced by RunWithContext, is returned as-is.
func (p *Predicate) match(ctx context.Context, input any) (bool, error) {
	iter := p.code.RunWithContext(ctx, input)
	for {
		v, ok := iter.Next()
		if !ok {
			return false, nil
		}
		if err, ok := v.(error); ok {
			return false, err
		}
		if v != nil && v != false {
			return true, nil
		}
	}
}
