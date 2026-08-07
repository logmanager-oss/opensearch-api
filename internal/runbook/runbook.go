// Package runbook loads a YAML-defined runbook: an ordered sequence of
// OpenSearch calls. Load parses, defaults, and validates each call, then
// resolves cross-call references (verify-with/depends-on) so the resulting
// Runbook is guaranteed executable.
package runbook

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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
	Entries []int  // indexes of entry calls (calls not targeted by any verify-with), document order
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
// parsed, defaulted (from config.Defaults().Retry) and validated on its own,
// then cross-call rules resolve verify-with/depends-on targets, reject
// cycles, and populate Entries so the result is guaranteed executable.
// baseDir is the directory relative @file body paths are resolved against
// (the runbook file's own directory); "" resolves them against the process
// cwd.
func Load(r io.Reader, baseDir string) (*Runbook, error) {
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

		call, err := decodeCall(name, line, valNode, baseDir)
		if err != nil {
			return nil, err
		}

		rb.byName[name] = len(rb.Calls)
		rb.Calls = append(rb.Calls, call)
	}

	if err := rb.validate(); err != nil {
		return nil, err
	}

	return rb, nil
}

// validate applies the cross-call rules to a fully-decoded Runbook: it
// resolves verify-with/depends-on targets, rejects cycles, and populates
// Entries. It must run only after every call has been decoded, since it
// relies on rb.byName covering the whole document.
func (rb *Runbook) validate() error {
	checkOnly := make(map[string]bool, len(rb.Calls))
	for i := range rb.Calls {
		call := &rb.Calls[i]
		if call.VerifyWith == "" {
			continue
		}
		if _, ok := rb.byName[call.VerifyWith]; !ok {
			return fmt.Errorf("call %q: verify-with %q: not a defined call", call.Name, call.VerifyWith)
		}
		checkOnly[call.VerifyWith] = true
	}

	rb.Entries = make([]int, 0, len(rb.Calls))
	for i := range rb.Calls {
		if !checkOnly[rb.Calls[i].Name] {
			rb.Entries = append(rb.Entries, i)
		}
	}
	if len(rb.Entries) == 0 {
		return errors.New("runbook: no entry calls: every call is a verify-with target")
	}

	for i := range rb.Calls {
		if err := rb.validateDependsOn(i, checkOnly); err != nil {
			return err
		}
	}

	for i := range rb.Calls {
		call := &rb.Calls[i]
		if !checkOnly[call.Name] {
			continue
		}
		if len(call.DependsOn) > 0 {
			return fmt.Errorf("call %q: check-only calls (verify-with targets) may not have depends-on", call.Name)
		}
		if call.StopOnFailure {
			return fmt.Errorf("call %q: check-only calls (verify-with targets) may not have stop-on-failure", call.Name)
		}
	}

	return rb.detectVerifyWithCycles()
}

// validateDependsOn checks every depends-on target of the call at index i:
// it must be a defined call, defined earlier in the document (forward
// references would allow cycles), and an entry call rather than a check-only
// one.
func (rb *Runbook) validateDependsOn(i int, checkOnly map[string]bool) error {
	call := &rb.Calls[i]
	seen := make(map[string]bool, len(call.DependsOn))
	for _, dep := range call.DependsOn {
		if seen[dep] {
			return fmt.Errorf("call %q: depends-on %q: listed more than once", call.Name, dep)
		}
		seen[dep] = true
		depIdx, ok := rb.byName[dep]
		if !ok {
			return fmt.Errorf("call %q: depends-on %q: not a defined call", call.Name, dep)
		}
		if depIdx >= i {
			return fmt.Errorf("call %q: depends-on %q: must be defined earlier in the document", call.Name, dep)
		}
		if checkOnly[dep] {
			return fmt.Errorf("call %q: depends-on %q: target is a check-only call (a verify-with target)", call.Name, dep)
		}
	}
	return nil
}

// detectVerifyWithCycles follows the single-edge verify-with chain from each
// call and rejects self-cycles (a -> a) and longer cycles (a -> b -> a).
// Chains without a cycle (a -> b -> c) are legal.
func (rb *Runbook) detectVerifyWithCycles() error {
	for i := range rb.Calls {
		start := rb.Calls[i].Name
		visited := map[string]bool{start: true}
		cur := rb.Calls[i].VerifyWith
		for cur != "" {
			if visited[cur] {
				return fmt.Errorf("call %q: verify-with cycle detected", start)
			}
			visited[cur] = true
			cur = rb.Calls[rb.byName[cur]].VerifyWith
		}
	}
	return nil
}

// decodeCall validates node's keys, decodes it into a callSpec, and builds
// the resulting Call.
func decodeCall(name string, line int, node *yaml.Node, baseDir string) (Call, error) {
	if node.Kind == yaml.AliasNode {
		return Call{}, fmt.Errorf("call %q (line %d): YAML aliases are not supported", name, line)
	}
	if node.Kind != yaml.MappingNode {
		return Call{}, fmt.Errorf("call %q (line %d): must be a mapping", name, line)
	}

	var hasSuccessWhen, hasVerifyWith bool
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if !allowedCallKeys[key.Value] {
			return Call{}, fmt.Errorf("call %q (line %d): unknown key %q", name, key.Line, key.Value)
		}
		hasSuccessWhen = hasSuccessWhen || key.Value == "success-when"
		hasVerifyWith = hasVerifyWith || key.Value == "verify-with"
	}
	// Checked on key presence, not decoded values: the spec fields are
	// pre-populated from defaults below, so the values alone cannot tell what
	// the user actually wrote.
	if hasSuccessWhen && hasVerifyWith {
		return Call{}, fmt.Errorf("call %q (line %d): success-when and verify-with are mutually exclusive", name, line)
	}

	// Pre-populate from the flag-mode defaults so a key absent from the YAML
	// keeps the default even if that default ever stops being the zero value;
	// Decode only touches fields whose keys are present. String-encoded fields
	// get the defaults' string forms so buildRetryConfig can parse them
	// unconditionally, making an explicit empty value an error as in flag mode.
	def := config.Defaults().Retry
	spec := callSpec{
		Retry:          def.MaxRetries,
		Backoff:        def.Strategy.String(),
		BackoffInitial: def.Initial.String(),
		BackoffMax:     def.Max.String(),
		BackoffJitter:  def.Jitter,
		AbortOn:        def.AbortOn,
		RetryWhen:      def.RetryWhen,
		SuccessWhen:    def.SuccessWhen,
		MaxBodyBuffer:  config.FormatSize(def.MaxBodyBuffer),
	}
	if err := node.Decode(&spec); err != nil {
		return Call{}, fmt.Errorf("call %q (line %d): decoding: %w", name, line, err)
	}

	return buildCall(name, line, &spec, baseDir)
}

// buildCall validates spec and resolves it into a Call: reads/rejects the
// body (relative @file paths resolve against baseDir, the runbook file's
// directory), parses the string-encoded retry fields, and compiles the jq
// predicates.
func buildCall(name string, line int, spec *callSpec, baseDir string) (Call, error) {
	if spec.Path == "" {
		return Call{}, fmt.Errorf("call %q (line %d): path is required", name, line)
	}

	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}

	// Relative @file paths resolve against the runbook file's directory
	// (baseDir), not the process cwd; "@-" and absolute paths are untouched.
	bodyArg := spec.Body
	if path, ok := strings.CutPrefix(bodyArg, "@"); ok && path != "-" && !filepath.IsAbs(path) && baseDir != "" {
		bodyArg = "@" + filepath.Join(baseDir, path)
	}

	// nil stdin: run-file bodies never read from the process's stdin, so "@-"
	// always fails with osclient.ErrNoStdin.
	body, hasBody, err := osclient.ReadBody(bodyArg, nil)
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

// buildRetryConfig parses spec's retry fields into a config.RetryConfig. The
// string-encoded fields were pre-populated with the flag-mode defaults'
// string forms in decodeCall, so they are parsed unconditionally here: an
// explicit empty value fails exactly as it would in flag mode.
func buildRetryConfig(name string, line int, spec *callSpec) (config.RetryConfig, error) {
	cfg := config.Defaults().Retry

	cfg.MaxRetries = spec.Retry
	cfg.AbortOn = spec.AbortOn
	cfg.RetryWhen = spec.RetryWhen
	cfg.SuccessWhen = spec.SuccessWhen
	cfg.Jitter = spec.BackoffJitter

	strategy, err := config.ParseBackoffStrategy(spec.Backoff)
	if err != nil {
		return config.RetryConfig{}, fmt.Errorf("call %q (line %d): backoff: %w", name, line, err)
	}
	cfg.Strategy = strategy

	initial, err := time.ParseDuration(spec.BackoffInitial)
	if err != nil {
		return config.RetryConfig{}, fmt.Errorf("call %q (line %d): backoff-initial: %w", name, line, err)
	}
	cfg.Initial = initial

	maxBackoff, err := time.ParseDuration(spec.BackoffMax)
	if err != nil {
		return config.RetryConfig{}, fmt.Errorf("call %q (line %d): backoff-max: %w", name, line, err)
	}
	cfg.Max = maxBackoff

	size, err := config.ParseSize(spec.MaxBodyBuffer)
	if err != nil {
		return config.RetryConfig{}, fmt.Errorf("call %q (line %d): max-body-buffer: %w", name, line, err)
	}
	cfg.MaxBodyBuffer = size

	return cfg, nil
}
