// Package runbook loads declarative, multi-call YAML runbooks: a sequence of
// OpenSearch calls, each mirroring the osapi request flags, executed in
// document order.
package runbook

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// Call is one request in a runbook, fully resolved: body read, durations and
// sizes parsed, predicates compiled. Nothing here requires further parsing
// before the call can be executed. RetryWhen/SuccessWhen are the compiled form
// of the expressions Retry also carries as strings; execute the compiled ones
// rather than recompiling from Retry.
type Call struct {
	Name, Method, Path string
	Body               []byte
	HasBody            bool
	Query              map[string]string
	Headers            http.Header
	Retry              config.RetryConfig
	RetryWhen          *retry.Predicate
	SuccessWhen        *retry.Predicate
	ContinueOnFailure  bool
}

// Runbook stays a struct rather than a bare []Call so a future top-level key
// (credentials:, vars:) is an added field, not a signature change.
type Runbook struct {
	Calls []Call // document order; all of them run
}

// callSpec is the YAML decode target for a call or the defaults block.
// Durations and sizes decode as strings: yaml.v3 does not parse "2s" into
// time.Duration, so they are parsed via time.ParseDuration / config.ParseSize
// after decode. "capture" has no field here on purpose — Section 3 adds its
// name-to-jq parsing; for now it is accepted as a key and its value ignored.
type callSpec struct {
	Name              string            `yaml:"name"`
	Method            string            `yaml:"method"`
	Path              string            `yaml:"path"`
	Body              string            `yaml:"body"`
	Query             map[string]string `yaml:"query"`
	Headers           map[string]string `yaml:"headers"`
	Retry             int               `yaml:"retry"`
	Backoff           string            `yaml:"backoff"`
	BackoffInitial    string            `yaml:"backoff-initial"`
	BackoffMax        string            `yaml:"backoff-max"`
	BackoffJitter     float64           `yaml:"backoff-jitter"`
	AbortOn           []int             `yaml:"abort-on"`
	RetryWhen         string            `yaml:"retry-when"`
	SuccessWhen       string            `yaml:"success-when"`
	ContinueOnFailure bool              `yaml:"continue-on-failure"`
	MaxBodyBuffer     string            `yaml:"max-body-buffer"`
}

// Load parses a runbook. Relative @file body paths resolve against baseDir,
// which the caller sets to the runbook file's own directory; an empty baseDir
// leaves them relative to the process working directory.
func Load(r io.Reader, baseDir string) (*Runbook, error) {
	var doc struct {
		Defaults yaml.Node `yaml:"defaults"`
		Calls    yaml.Node `yaml:"calls"`
	}
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	// An empty input is io.EOF here; fall through so validateCallsShape gives
	// "calls: is required" instead of a bare EOF.
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("loading runbook: %w", err)
	}
	if err := rejectExtraDocuments(dec); err != nil {
		return nil, err
	}

	defaultsSpec, err := loadDefaults(&doc.Defaults, baseDir)
	if err != nil {
		return nil, err
	}

	if err := validateCallsShape(&doc.Calls); err != nil {
		return nil, err
	}
	// Duplicate names are a purely structural property, so check them before
	// per-call loading reads body files and compiles jq: otherwise which error
	// a duplicated call reports depends on the filesystem.
	if err := rejectDuplicateNames(doc.Calls.Content); err != nil {
		return nil, err
	}

	calls := make([]Call, len(doc.Calls.Content))
	for i, item := range doc.Calls.Content {
		call, err := loadCall(item, baseDir, &defaultsSpec)
		if err != nil {
			return nil, err
		}
		calls[i] = call
	}

	return &Runbook{Calls: calls}, nil
}

// LoadFile opens path and loads it with baseDir = filepath.Dir(path). The CLI
// uses this so the two can never be paired wrongly; tests use Load directly.
func LoadFile(path string) (*Runbook, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening runbook %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	return Load(f, filepath.Dir(path))
}

// rejectExtraDocuments fails a runbook split across "---" separators. A single
// Decode reads only the first document, so accepting the file would silently
// drop every call after the separator.
func rejectExtraDocuments(dec *yaml.Decoder) error {
	var extra yaml.Node
	err := dec.Decode(&extra)
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return fmt.Errorf("loading runbook: %w", err)
	default:
		return fmt.Errorf("line %d: a runbook must be a single YAML document; "+
			"calls after a \"---\" separator would not run", extra.Line)
	}
}

// rejectDuplicateNames fails two calls sharing a name, naming both lines.
// Unnamed calls are skipped: the missing-name error is per-call.
func rejectDuplicateNames(items []*yaml.Node) error {
	seen := make(map[string]int, len(items))
	for _, item := range items {
		name := rawStringField(item, "name")
		if name == "" {
			continue
		}
		if prevLine, ok := seen[name]; ok {
			return fmt.Errorf("call %q (line %d): duplicate of the call at line %d",
				name, item.Line, prevLine)
		}
		seen[name] = item.Line
	}
	return nil
}

// callAllowedKeys are the per-call keys accepted by the loader. "capture" is
// accepted here so a runbook using it still loads; Section 3 adds its
// name-to-jq parsing and validation.
var callAllowedKeys = map[string]bool{
	"name": true, "method": true, "path": true, "body": true,
	"query": true, "headers": true, "retry": true, "backoff": true,
	"backoff-initial": true, "backoff-max": true, "backoff-jitter": true,
	"abort-on": true, "retry-when": true, "success-when": true,
	"capture": true, "continue-on-failure": true, "max-body-buffer": true,
}

// defaultsAllowedKeys are callAllowedKeys minus name, path and capture: a
// default applies to every call, so a per-call identity/capture key makes no
// sense there.
var defaultsAllowedKeys = func() map[string]bool {
	m := maps.Clone(callAllowedKeys)
	delete(m, "name")
	delete(m, "path")
	delete(m, "capture")
	return m
}()

// loadDefaults decodes the optional defaults: block into a callSpec baseline
// that every call's spec starts from, and validates it exactly as a call's
// own spec is validated: backoff/durations/size parse, retry-when/success-when
// compile, and an "@file" body resolves against baseDir. Without this, an
// invalid default was only ever parsed per-call, which meant an error landed
// on the first call that happened not to override the bad key (misattributing
// it) or never surfaced at all if every call happened to override it (letting
// it escape). A missing defaults: yields the zero callSpec, i.e. no defaults.
func loadDefaults(node *yaml.Node, baseDir string) (callSpec, error) {
	if node.Kind == 0 {
		return callSpec{}, nil
	}

	ref := fmt.Sprintf("defaults (line %d)", node.Line)
	if node.Kind != yaml.MappingNode {
		return callSpec{}, fmt.Errorf("%s: must be a mapping", ref)
	}
	if err := checkAllowedKeys(node, defaultsAllowedKeys, ref); err != nil {
		return callSpec{}, err
	}

	var spec callSpec
	if err := node.Decode(&spec); err != nil {
		return callSpec{}, fmt.Errorf("%s: %w", ref, err)
	}

	if err := validateMethod(spec.Method); err != nil {
		return callSpec{}, fmt.Errorf("%s: %w", ref, err)
	}
	if _, err := buildRetryConfig(&spec); err != nil {
		return callSpec{}, fmt.Errorf("%s: %w", ref, err)
	}
	if _, err := retry.CompilePredicate(spec.RetryWhen); err != nil {
		return callSpec{}, fmt.Errorf("%s: retry-when: %w", ref, err)
	}
	if _, err := retry.CompilePredicate(spec.SuccessWhen); err != nil {
		return callSpec{}, fmt.Errorf("%s: success-when: %w", ref, err)
	}
	bodyArg, err := resolveBodyArg(spec.Body, baseDir)
	if err != nil {
		return callSpec{}, fmt.Errorf("%s: %w", ref, err)
	}
	// nil stdin: same reasoning as loadCall — a runbook has no stdin of its
	// own, so "@-" in defaults is rejected the same way it would be per-call.
	if _, _, err := osclient.ReadBody(bodyArg, nil); err != nil {
		return callSpec{}, fmt.Errorf("%s: reading body: %w", ref, err)
	}

	return spec, nil
}

func validateCallsShape(calls *yaml.Node) error {
	if calls.Kind == 0 {
		return errors.New("calls: is required")
	}
	if calls.Tag == "!!null" {
		return fmt.Errorf("calls (line %d): must not be empty", calls.Line)
	}
	if calls.Kind != yaml.SequenceNode {
		return fmt.Errorf("calls (line %d): must be a sequence, got %s", calls.Line, nodeKindName(calls.Kind))
	}
	if len(calls.Content) == 0 {
		return fmt.Errorf("calls (line %d): must not be empty", calls.Line)
	}
	return nil
}

func nodeKindName(k yaml.Kind) string {
	switch k {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "an unknown node"
	}
}

func loadCall(item *yaml.Node, baseDir string, defaults *callSpec) (Call, error) {
	name := rawStringField(item, "name")
	ref := callRef(name, item.Line)

	// Checked before rawStringField's result is trusted any further: both it
	// and checkAllowedKeys read .Content as key/value pairs, which only a
	// mapping node is.
	if item.Kind != yaml.MappingNode {
		return Call{}, fmt.Errorf("call (line %d): must be a mapping, got %s", item.Line, nodeKindName(item.Kind))
	}
	if err := checkAllowedKeys(item, callAllowedKeys, ref); err != nil {
		return Call{}, err
	}

	// Decoding onto a copy of defaults is what layers them: yaml.v3 leaves
	// fields absent from the node untouched. Two consequences worth knowing:
	// a call cannot reset an inherited key to its zero value (retry: 0 reads
	// the same as omitted), and yaml.v3 decodes a mapping into an existing
	// non-nil map *in place*, so Query is cloned — sharing it would leak one
	// call's overrides into the next and into defaults itself. Query therefore
	// merges with the defaults this way; slices such as abort-on replace them.
	// Headers merge separately below, by canonical HTTP header name rather
	// than by raw map key: decode-in-place would keep "content-type" (from
	// defaults) and "Content-Type" (from the call) as two distinct keys, and
	// http.Header.Set calls on both would let map-iteration order pick the
	// winner.
	spec := *defaults
	spec.Query = maps.Clone(defaults.Query)
	spec.Headers = nil
	if err := item.Decode(&spec); err != nil {
		return Call{}, fmt.Errorf("%s: %w", ref, err)
	}
	headers := mergeHeaders(defaults.Headers, spec.Headers)
	if spec.Name == "" {
		return Call{}, fmt.Errorf("%s: missing required key %q", ref, "name")
	}
	if spec.Path == "" {
		return Call{}, fmt.Errorf("%s: missing required key %q", ref, "path")
	}
	if err := validateMethod(spec.Method); err != nil {
		return Call{}, fmt.Errorf("%s: %w", ref, err)
	}

	bodyArg, err := resolveBodyArg(spec.Body, baseDir)
	if err != nil {
		return Call{}, fmt.Errorf("%s: %w", ref, err)
	}
	// nil stdin: a runbook has no stdin of its own, which is what makes
	// osclient.ReadBody reject "@-" with ErrNoStdin.
	body, hasBody, err := osclient.ReadBody(bodyArg, nil)
	if err != nil {
		return Call{}, fmt.Errorf("%s: reading body: %w", ref, err)
	}

	retryCfg, err := buildRetryConfig(&spec)
	if err != nil {
		return Call{}, fmt.Errorf("%s: %w", ref, err)
	}

	retryWhen, err := retry.CompilePredicate(spec.RetryWhen)
	if err != nil {
		return Call{}, fmt.Errorf("%s: retry-when: %w", ref, err)
	}
	successWhen, err := retry.CompilePredicate(spec.SuccessWhen)
	if err != nil {
		return Call{}, fmt.Errorf("%s: success-when: %w", ref, err)
	}

	return Call{
		Name:              spec.Name,
		Method:            spec.Method,
		Path:              spec.Path,
		Body:              body,
		HasBody:           hasBody,
		Query:             spec.Query,
		Headers:           headers,
		Retry:             retryCfg,
		RetryWhen:         retryWhen,
		SuccessWhen:       successWhen,
		ContinueOnFailure: spec.ContinueOnFailure,
	}, nil
}

// buildRetryConfig resolves the parsed spec into config.RetryConfig, falling
// back to config.Defaults().Retry per-field for anything left as its zero
// value so run mode and flag mode can't drift.
func buildRetryConfig(spec *callSpec) (config.RetryConfig, error) {
	defaults := config.Defaults().Retry
	var err error

	strategy := defaults.Strategy
	if spec.Backoff != "" {
		strategy, err = config.ParseBackoffStrategy(spec.Backoff)
		if err != nil {
			return config.RetryConfig{}, fmt.Errorf("backoff: %w", err)
		}
	}

	initial := defaults.Initial
	if spec.BackoffInitial != "" {
		initial, err = time.ParseDuration(spec.BackoffInitial)
		if err != nil {
			return config.RetryConfig{}, fmt.Errorf("backoff-initial: %w", err)
		}
	}

	maxDelay := defaults.Max
	if spec.BackoffMax != "" {
		maxDelay, err = time.ParseDuration(spec.BackoffMax)
		if err != nil {
			return config.RetryConfig{}, fmt.Errorf("backoff-max: %w", err)
		}
	}

	maxBodyBuffer := defaults.MaxBodyBuffer
	if spec.MaxBodyBuffer != "" {
		maxBodyBuffer, err = config.ParseSize(spec.MaxBodyBuffer)
		if err != nil {
			return config.RetryConfig{}, fmt.Errorf("max-body-buffer: %w", err)
		}
	}

	return config.RetryConfig{
		MaxRetries: spec.Retry,
		Strategy:   strategy,
		Initial:    initial,
		Max:        maxDelay,
		Jitter:     spec.BackoffJitter,
		// Cloned for the same reason config.Resolve clones it: every call that
		// inherits abort-on would otherwise share one backing array.
		AbortOn:       slices.Clone(spec.AbortOn),
		RetryWhen:     spec.RetryWhen,
		SuccessWhen:   spec.SuccessWhen,
		MaxBodyBuffer: maxBodyBuffer,
	}, nil
}

// validateMethod rejects a method token http.NewRequest would reject at
// execution time. Without this, an invalid method (e.g. "GET EXTRA") loaded
// cleanly and only failed when osclient.BuildRequest called http.NewRequest
// mid-run — aborting after earlier calls had already mutated the cluster,
// which is exactly what load-time validation exists to prevent. The probe
// request reuses net/http's own token check rather than reimplementing RFC
// 7230's token grammar, so load-time acceptance guarantees execution-time
// acceptance; the URL and body are irrelevant to that check. An empty method
// is left for http.NewRequest to default to GET, same as today.
func validateMethod(method string) error {
	if method == "" {
		return nil
	}
	if _, err := http.NewRequest(method, "http://probe/", http.NoBody); err != nil {
		return fmt.Errorf("method: %w", err)
	}
	return nil
}

// mergeHeaders layers own onto defaults by canonical HTTP header name
// (textproto.CanonicalMIMEHeaderKey, applied via http.Header.Set), so
// "Content-Type" in own always beats "content-type" in defaults — or any
// other case spelling either side happens to use — rather than leaving the
// winner to map iteration order. Unlike Query, headers cannot merge as plain
// map[string]string: two spellings of the same header are distinct map keys
// but not distinct headers.
func mergeHeaders(defaults, own map[string]string) http.Header {
	if len(defaults) == 0 && len(own) == 0 {
		return nil
	}
	h := make(http.Header, len(defaults)+len(own))
	for k, v := range defaults {
		h.Set(k, v)
	}
	for k, v := range own {
		h.Set(k, v)
	}
	return h
}

// resolveBodyArg rewrites a relative "@file" body argument against baseDir so
// osclient.ReadBody sees an absolute-enough path. "@-" (stdin), literal
// bodies and already-absolute "@file" paths pass through untouched.
func resolveBodyArg(arg, baseDir string) (string, error) {
	if arg == "" || arg[0] != '@' || arg == "@-" {
		return arg, nil
	}
	path := arg[1:]
	if path == "" {
		return "", errors.New(`body: "@" needs a file path`)
	}
	if filepath.IsAbs(path) {
		return arg, nil
	}
	return "@" + filepath.Join(baseDir, path), nil
}

// callRef names a call for error messages: by name and line when the name
// parsed, or by line alone when it did not (e.g. name: is missing).
func callRef(name string, line int) string {
	if name == "" {
		return fmt.Sprintf("call (line %d)", line)
	}
	return fmt.Sprintf("call %q (line %d)", name, line)
}

// rawStringField reads a scalar key's value straight from a mapping node,
// ahead of the allowed-key check and struct decode, so error messages can
// name the call even when its keys are invalid.
func rawStringField(node *yaml.Node, key string) string {
	if node.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value
		}
	}
	return ""
}

// checkAllowedKeys rejects a mapping node with any key not in allowed, so a
// typo (e.g. succes-when) fails to load instead of being silently ignored —
// node.Decode does not honor Decoder.KnownFields.
func checkAllowedKeys(node *yaml.Node, allowed map[string]bool, ref string) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !allowed[key] {
			return fmt.Errorf("%s: unknown key %q", ref, key)
		}
	}
	return nil
}
