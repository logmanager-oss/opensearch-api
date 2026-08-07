// Package runbook loads a YAML-defined runbook: an ordered sequence of
// OpenSearch calls. Cross-call validation (resolving verify-with/depends-on
// targets and populating Entries) is a later section; Load only parses,
// defaults, and validates each call in isolation.
package runbook

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// callsKey is the sole top-level YAML key a runbook document may contain.
const callsKey = "calls"

// Call is one HTTP call within a runbook.
type Call struct {
	Name, Method, Path string
	Body               []byte
	HasBody            bool
	Query              map[string]string
	Headers            http.Header
	Retry              config.RetryConfig
	RetryWhen          *retry.Predicate // compiled at load
	SuccessWhen        *retry.Predicate
	VerifyWith         string   // "" = none; names a check-only target
	DependsOn          []string // earlier entry calls
	StopOnFailure      bool
}

// Runbook is an ordered set of calls loaded from YAML.
type Runbook struct {
	Calls   []Call // document order
	Entries []int  // indexes of entry calls, document order (populated by cross-call validation, a later section)
	byName  map[string]int
}

// document is the top-level shape of a runbook file. Calls is decoded as a
// raw yaml.Node (rather than a map) so its key nodes retain .Line for error
// anchoring and so calls can be walked in document order.
type document struct {
	Calls yaml.Node `yaml:"calls"`
}

// callSpec mirrors the per-call YAML keys. Durations and sizes are decoded as
// strings, not time.Duration/int64: yaml.v3 has no notion of time.Duration
// and does not parse "2s" into one, so these fields are parsed explicitly via
// time.ParseDuration / config.ParseSize after decoding.
type callSpec struct {
	Path           string            `yaml:"path"`
	Method         string            `yaml:"method"`
	Body           string            `yaml:"body"`
	Query          map[string]string `yaml:"query"`
	Headers        map[string]string `yaml:"headers"`
	Retry          int               `yaml:"retry"`
	Backoff        string            `yaml:"backoff"`
	BackoffInitial string            `yaml:"backoff-initial"`
	BackoffMax     string            `yaml:"backoff-max"`
	BackoffJitter  float64           `yaml:"backoff-jitter"`
	AbortOn        []int             `yaml:"abort-on"`
	RetryWhen      string            `yaml:"retry-when"`
	SuccessWhen    string            `yaml:"success-when"`
	VerifyWith     string            `yaml:"verify-with"`
	DependsOn      dependsOn         `yaml:"depends-on"`
	StopOnFailure  bool              `yaml:"stop-on-failure"`
	MaxBodyBuffer  string            `yaml:"max-body-buffer"`
}

// allowedCallKeys is checked against a call's keys before decoding into
// callSpec: node.Decode into a struct does not honour KnownFields, so a typo
// like "succes-when" would otherwise decode silently instead of erroring.
var allowedCallKeys = map[string]bool{
	"path":            true,
	"method":          true,
	"body":            true,
	"query":           true,
	"headers":         true,
	"retry":           true,
	"backoff":         true,
	"backoff-initial": true,
	"backoff-max":     true,
	"backoff-jitter":  true,
	"abort-on":        true,
	"retry-when":      true,
	"success-when":    true,
	"verify-with":     true,
	"depends-on":      true,
	"stop-on-failure": true,
	"max-body-buffer": true,
}

// dependsOn accepts either a scalar call name or a sequence of call names.
type dependsOn []string

// UnmarshalYAML implements yaml.Unmarshaler so depends-on can be either a
// bare scalar or a sequence, dispatching on the node's kind.
func (d *dependsOn) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*d = dependsOn{s}
		return nil
	case yaml.SequenceNode:
		var s []string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*d = dependsOn(s)
		return nil
	default:
		return fmt.Errorf("depends-on: want a call name or a list of call names")
	}
}

// Load parses a runbook YAML document into an ordered Runbook. Each call is
// parsed, defaulted (from config.Defaults().Retry) and validated on its own;
// verify-with/depends-on targets are stored as given but not resolved, and
// Entries is left unpopulated — both are cross-call validation, a later
// section.
func Load(r io.Reader) (*Runbook, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var doc document
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("runbook is empty")
		}
		return nil, fmt.Errorf("decoding runbook: %w", err)
	}
	// A second document would otherwise be silently dropped (Decode reads one
	// document per call).
	switch err := dec.Decode(&document{}); {
	case err == nil:
		return nil, errors.New("runbook must contain a single YAML document")
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("reading past first document: %w", err)
	}

	if doc.Calls.Kind == 0 {
		return nil, fmt.Errorf("runbook: missing required key %q", callsKey)
	}
	if doc.Calls.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("runbook: %q (line %d) must be a mapping of call name to call spec",
			callsKey, doc.Calls.Line)
	}

	rb := &Runbook{
		Calls:  make([]Call, 0, len(doc.Calls.Content)/2),
		byName: make(map[string]int, len(doc.Calls.Content)/2),
	}

	// yaml.v3 does not enforce mapping-key uniqueness when decoding into a
	// yaml.Node, so duplicate call names must be checked explicitly here.
	seen := make(map[string]bool, len(doc.Calls.Content)/2)
	for i := 0; i < len(doc.Calls.Content); i += 2 {
		keyNode, valNode := doc.Calls.Content[i], doc.Calls.Content[i+1]
		name, line := keyNode.Value, keyNode.Line

		if keyNode.Kind != yaml.ScalarNode || name == "" {
			return nil, fmt.Errorf("line %d: call name must be a non-empty scalar", line)
		}
		if seen[name] {
			return nil, fmt.Errorf("call %q (line %d): duplicate call name", name, line)
		}
		seen[name] = true

		call, err := decodeCall(name, line, valNode)
		if err != nil {
			return nil, err
		}

		rb.byName[name] = len(rb.Calls)
		rb.Calls = append(rb.Calls, call)
	}

	return rb, nil
}

// decodeCall validates node's keys, decodes it into a callSpec, and builds
// the resulting Call.
func decodeCall(name string, line int, node *yaml.Node) (Call, error) {
	if node.Kind == yaml.AliasNode {
		return Call{}, fmt.Errorf("call %q (line %d): YAML aliases are not supported", name, line)
	}
	if node.Kind != yaml.MappingNode {
		return Call{}, fmt.Errorf("call %q (line %d): must be a mapping", name, line)
	}

	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if !allowedCallKeys[key.Value] {
			return Call{}, fmt.Errorf("call %q (line %d): unknown key %q", name, key.Line, key.Value)
		}
	}

	// Pre-populate from the flag-mode defaults so a key absent from the YAML
	// keeps the default even if that default ever stops being the zero value;
	// Decode only touches fields whose keys are present.
	def := config.Defaults().Retry
	spec := callSpec{
		Retry:         def.MaxRetries,
		BackoffJitter: def.Jitter,
		AbortOn:       def.AbortOn,
		RetryWhen:     def.RetryWhen,
		SuccessWhen:   def.SuccessWhen,
	}
	if err := node.Decode(&spec); err != nil {
		return Call{}, fmt.Errorf("call %q (line %d): decoding: %w", name, line, err)
	}

	return buildCall(name, line, &spec)
}

// buildCall validates spec and resolves it into a Call: reads/rejects the
// body, parses the string-encoded retry fields, and compiles the jq
// predicates.
func buildCall(name string, line int, spec *callSpec) (Call, error) {
	if spec.Path == "" {
		return Call{}, fmt.Errorf("call %q (line %d): path is required", name, line)
	}

	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}

	// nil stdin: run-file bodies never read from the process's stdin, so "@-"
	// always fails with osclient.ErrNoStdin.
	body, hasBody, err := osclient.ReadBody(spec.Body, nil)
	if err != nil {
		return Call{}, fmt.Errorf("call %q (line %d): reading body: %w", name, line, err)
	}

	var headers http.Header
	if len(spec.Headers) > 0 {
		headers = make(http.Header, len(spec.Headers))
		for k, v := range spec.Headers {
			// Case-variant keys ("content-type" and "Content-Type") are
			// distinct in YAML but collapse to one canonical header; which
			// value survived would depend on map iteration order.
			if _, ok := headers[http.CanonicalHeaderKey(k)]; ok {
				return Call{}, fmt.Errorf("call %q (line %d): header %q given more than once with different casing", name, line, k)
			}
			headers.Set(k, v)
		}
	}

	retryCfg, err := buildRetryConfig(name, line, spec)
	if err != nil {
		return Call{}, err
	}

	retryWhen, err := retry.CompilePredicate(spec.RetryWhen)
	if err != nil {
		return Call{}, fmt.Errorf("call %q (line %d): retry-when: %w", name, line, err)
	}
	successWhen, err := retry.CompilePredicate(spec.SuccessWhen)
	if err != nil {
		return Call{}, fmt.Errorf("call %q (line %d): success-when: %w", name, line, err)
	}

	return Call{
		Name:          name,
		Method:        method,
		Path:          spec.Path,
		Body:          body,
		HasBody:       hasBody,
		Query:         spec.Query,
		Headers:       headers,
		Retry:         retryCfg,
		RetryWhen:     retryWhen,
		SuccessWhen:   successWhen,
		VerifyWith:    spec.VerifyWith,
		DependsOn:     spec.DependsOn,
		StopOnFailure: spec.StopOnFailure,
	}, nil
}

// buildRetryConfig starts from config.Defaults().Retry so an omitted per-call
// key can never drift from the flag-mode default, then overrides only the
// keys spec sets explicitly.
func buildRetryConfig(name string, line int, spec *callSpec) (config.RetryConfig, error) {
	cfg := config.Defaults().Retry

	cfg.MaxRetries = spec.Retry
	cfg.AbortOn = spec.AbortOn
	cfg.RetryWhen = spec.RetryWhen
	cfg.SuccessWhen = spec.SuccessWhen
	cfg.Jitter = spec.BackoffJitter

	if spec.Backoff != "" {
		strategy, err := config.ParseBackoffStrategy(spec.Backoff)
		if err != nil {
			return config.RetryConfig{}, fmt.Errorf("call %q (line %d): backoff: %w", name, line, err)
		}
		cfg.Strategy = strategy
	}

	if spec.BackoffInitial != "" {
		d, err := time.ParseDuration(spec.BackoffInitial)
		if err != nil {
			return config.RetryConfig{}, fmt.Errorf("call %q (line %d): backoff-initial: %w", name, line, err)
		}
		cfg.Initial = d
	}

	if spec.BackoffMax != "" {
		d, err := time.ParseDuration(spec.BackoffMax)
		if err != nil {
			return config.RetryConfig{}, fmt.Errorf("call %q (line %d): backoff-max: %w", name, line, err)
		}
		cfg.Max = d
	}

	if spec.MaxBodyBuffer != "" {
		size, err := config.ParseSize(spec.MaxBodyBuffer)
		if err != nil {
			return config.RetryConfig{}, fmt.Errorf("call %q (line %d): max-body-buffer: %w", name, line, err)
		}
		cfg.MaxBodyBuffer = size
	}

	return cfg, nil
}
