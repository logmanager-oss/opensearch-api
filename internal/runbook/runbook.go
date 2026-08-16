// Package runbook loads declarative, multi-call YAML runbooks: a sequence of
// OpenSearch calls, each mirroring the osapi request flags, executed in
// document order.
package runbook

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

// Call is one runbook request, fully resolved: body read, sizes and
// durations parsed, predicates compiled. Nothing here needs further parsing
// before execution. RetryWhen/SuccessWhen are the compiled form of Retry's
// string expressions. Execute these, not Retry's strings.
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
	Capture            []Capture // document order; empty when the call captures nothing
}

// References returns the capture names this call interpolates, across path,
// query values, header values and body, in sorted order. --dry-run prints
// them. It uses the same scanner Load validates with, so the two can't
// disagree.
func (c *Call) References() []string {
	names := make(map[string]struct{})
	collect := func(s string) {
		// Load already rejected any error scanTemplate could return here
		// (checkRefs), so it is dropped rather than propagated.
		_, _ = scanTemplate(s, func(name string) (string, error) {
			names[name] = struct{}{}
			return "", nil
		})
	}

	collect(c.Path)
	for _, v := range c.Query {
		collect(v)
	}
	for _, values := range c.Headers {
		for _, v := range values {
			collect(v)
		}
	}
	collect(string(c.Body))

	return slices.Sorted(maps.Keys(names))
}

// Capture is a name: jq-expression pair from a call's capture: mapping,
// holding both Expr (kept for error messages and --dry-run) and its
// compiled, unexported code. This package compiles its own rather than
// reusing retry.Predicate: Predicate loops for a truthy value, but a capture
// wants only the first value emitted.
type Capture struct {
	Name string
	Expr string
	code *gojq.Code
}

// capDecl records where a capture was declared, for validating ${name}
// references: the call's name (for errors) and whether it tolerates
// failure, which leaves the capture unset.
type capDecl struct {
	callName          string
	continueOnFailure bool
}

// Runbook stays a struct rather than a bare []Call so a future top-level key
// (vars:) is an added field, not a signature change.
type Runbook struct {
	Calls       []Call       // document order. All run
	Credentials *Credentials // defaults: credentials:, or nil when absent
}

// callSpec is the YAML decode target for a call or the defaults block.
// Durations and sizes decode as strings: yaml.v3 does not parse "2s" into
// time.Duration, so they are parsed via time.ParseDuration / config.ParseSize
// after decode. "capture" has no field here on purpose: a map field would not
// preserve document order, so loadCall parses it straight from the yaml.Node
// via rawNodeField/parseCaptures instead.
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
// which the caller sets to the runbook file's directory. An empty baseDir
// leaves them relative to the process working directory.
func Load(r io.Reader, baseDir string) (*Runbook, error) {
	var doc struct {
		Defaults yaml.Node `yaml:"defaults"`
		Calls    yaml.Node `yaml:"calls"`
	}
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	// An empty input reads as io.EOF here. Fall through so validateCallsShape
	// reports "calls: is required" instead of a bare EOF.
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("loading runbook: %w", err)
	}
	if err := rejectExtraDocuments(dec); err != nil {
		return nil, err
	}

	defaultsSpec, creds, err := loadDefaults(&doc.Defaults, baseDir)
	if err != nil {
		return nil, err
	}

	if err := validateCallsShape(&doc.Calls); err != nil {
		return nil, err
	}
	// Duplicate names are structural, so check them before per-call loading
	// reads files or compiles jq: otherwise which error a duplicate reports
	// depends on the filesystem.
	if err := rejectDuplicateNames(doc.Calls.Content); err != nil {
		return nil, err
	}

	// l.declared accumulates capture names in document order, so a ${name}
	// reference is checked only against earlier calls' captures. This rules
	// out forward references and cycles without a graph walk.
	l := &loader{
		baseDir:  baseDir,
		defaults: &defaultsSpec,
		declared: make(map[string]capDecl, len(doc.Calls.Content)),
	}
	calls := make([]Call, len(doc.Calls.Content))
	for i, item := range doc.Calls.Content {
		call, err := l.call(item)
		if err != nil {
			return nil, err
		}
		calls[i] = call
	}

	return &Runbook{Calls: calls, Credentials: creds}, nil
}

// loader carries the state Load threads through every call: baseDir and
// defaults are fixed for the run, declared accumulates as calls build.
// Bundling these keeps loader.call to one parameter.
type loader struct {
	baseDir  string
	defaults *callSpec
	declared map[string]capDecl
}

// LoadFile opens path and loads it with baseDir = filepath.Dir(path). The CLI
// uses this so the two can't be paired wrongly. Tests use Load directly.
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

// callAllowedKeys are the per-call keys accepted by the loader.
var callAllowedKeys = map[string]bool{
	"name": true, "method": true, "path": true, "body": true,
	"query": true, "headers": true, "retry": true, "backoff": true,
	"backoff-initial": true, "backoff-max": true, "backoff-jitter": true,
	"abort-on": true, "retry-when": true, "success-when": true,
	"capture": true, "continue-on-failure": true, "max-body-buffer": true,
}

// defaultsAllowedKeys is callAllowedKeys minus name, path and capture: a
// default applies to every call, so a per-call identity/capture key makes
// no sense there. It also adds credentials, a defaults-only key: a
// per-call credentials: key is rejected by callAllowedKeys instead.
var defaultsAllowedKeys = func() map[string]bool {
	m := maps.Clone(callAllowedKeys)
	delete(m, "name")
	delete(m, "path")
	delete(m, "capture")
	m["credentials"] = true
	return m
}()

// loadDefaults decodes the optional defaults: block into a callSpec baseline
// every call starts from, and validates it exactly like a call's own spec:
// backoff/durations/size parse, retry-when/success-when compile, and an
// "@file" body resolves against baseDir. Without this, an invalid default
// could be misattributed to a call, or escape validation entirely when every
// call overrode it. A missing defaults: yields the zero callSpec (no
// defaults).
func loadDefaults(node *yaml.Node, baseDir string) (callSpec, *Credentials, error) {
	if node.Kind == 0 {
		return callSpec{}, nil, nil
	}

	ref := fmt.Sprintf("defaults (line %d)", node.Line)
	if node.Kind != yaml.MappingNode {
		return callSpec{}, nil, fmt.Errorf("%s: must be a mapping", ref)
	}
	if err := checkAllowedKeys(node, defaultsAllowedKeys, ref); err != nil {
		return callSpec{}, nil, err
	}

	creds, err := parseCredentials(rawNodeField(node, "credentials"), ref)
	if err != nil {
		return callSpec{}, nil, err
	}

	var spec callSpec
	if err := node.Decode(&spec); err != nil {
		return callSpec{}, nil, fmt.Errorf("%s: %w", ref, err)
	}

	if err := validateMethod(spec.Method); err != nil {
		return callSpec{}, nil, fmt.Errorf("%s: %w", ref, err)
	}
	if _, err := buildRetryConfig(&spec); err != nil {
		return callSpec{}, nil, fmt.Errorf("%s: %w", ref, err)
	}
	if _, err := retry.CompilePredicate(spec.RetryWhen); err != nil {
		return callSpec{}, nil, fmt.Errorf("%s: retry-when: %w", ref, err)
	}
	if _, err := retry.CompilePredicate(spec.SuccessWhen); err != nil {
		return callSpec{}, nil, fmt.Errorf("%s: success-when: %w", ref, err)
	}
	bodyArg, err := resolveBodyArg(spec.Body, baseDir)
	if err != nil {
		return callSpec{}, nil, fmt.Errorf("%s: %w", ref, err)
	}
	// nil stdin: same reasoning as loadCall. A runbook has no stdin of its
	// own, so "@-" in defaults is rejected the same as per-call.
	if _, _, err := osclient.ReadBody(bodyArg, nil); err != nil {
		return callSpec{}, nil, fmt.Errorf("%s: reading body: %w", ref, err)
	}

	return spec, creds, nil
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

// call builds one Call from item, layering it onto l.defaults.
func (l *loader) call(item *yaml.Node) (Call, error) {
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

	// Decoding onto a copy of defaults layers them: yaml.v3 leaves absent
	// fields untouched, and a present key overrides even with a zero value
	// (retry: 0 beats an inherited 3). One gotcha: yaml.v3 decodes a mapping
	// in place, so Query is cloned to avoid leaking overrides into defaults.
	// Slices like abort-on simply replace instead of merging. Headers merge
	// separately below, by canonical HTTP name: decode-in-place would keep
	// "content-type" and "Content-Type" as distinct keys, leaving
	// http.Header.Set's winner to map-iteration order.
	spec := *l.defaults
	spec.Query = maps.Clone(l.defaults.Query)
	spec.Headers = nil
	if err := item.Decode(&spec); err != nil {
		return Call{}, fmt.Errorf("%s: %w", ref, err)
	}
	headers := mergeHeaders(l.defaults.Headers, spec.Headers)
	if spec.Name == "" {
		return Call{}, fmt.Errorf("%s: missing required key %q", ref, "name")
	}
	if spec.Path == "" {
		return Call{}, fmt.Errorf("%s: missing required key %q", ref, "path")
	}
	if err := validateMethod(spec.Method); err != nil {
		return Call{}, fmt.Errorf("%s: %w", ref, err)
	}

	captures, err := parseCaptures(rawNodeField(item, "capture"), ref)
	if err != nil {
		return Call{}, err
	}

	// path can never be defaults-inherited: defaultsAllowedKeys forbids it.
	if err := checkRefs(spec.Path, l.declared, ref, false); err != nil {
		return Call{}, err
	}

	queryNode := rawNodeField(item, "query")
	// Sorted so a call with several bad references always names the same one.
	for _, k := range slices.Sorted(maps.Keys(spec.Query)) {
		if err := rejectRefInKey(k, ref, "query"); err != nil {
			return Call{}, err
		}
		if err := checkRefs(spec.Query[k], l.declared, ref, !mappingHasKey(queryNode, k)); err != nil {
			return Call{}, err
		}
	}

	// Sorted for the same reason as the query loop above. Headers keep
	// defaults separate (mergeHeaders), so own and inherited values are
	// checked in two passes. An own key overriding a default canonically
	// skips it, since an overridden default never ships.
	for _, k := range slices.Sorted(maps.Keys(spec.Headers)) {
		if err := rejectRefInKey(k, ref, "header"); err != nil {
			return Call{}, err
		}
		if err := checkRefs(spec.Headers[k], l.declared, ref, false); err != nil {
			return Call{}, err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(l.defaults.Headers)) {
		if err := rejectRefInKey(k, ref, "header"); err != nil {
			return Call{}, err
		}
		if headerOverridden(spec.Headers, k) {
			continue
		}
		if err := checkRefs(l.defaults.Headers[k], l.declared, ref, true); err != nil {
			return Call{}, err
		}
	}

	bodyArg, err := resolveBodyArg(spec.Body, l.baseDir)
	if err != nil {
		return Call{}, fmt.Errorf("%s: %w", ref, err)
	}
	// nil stdin: a runbook has no stdin of its own, so osclient.ReadBody
	// rejects "@-" with ErrNoStdin.
	body, hasBody, err := osclient.ReadBody(bodyArg, nil)
	if err != nil {
		return Call{}, fmt.Errorf("%s: reading body: %w", ref, err)
	}
	bodyOwn := rawNodeField(item, "body") != nil
	if err := checkRefs(string(body), l.declared, bodyRef(ref, bodyArg), !bodyOwn); err != nil {
		return Call{}, err
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

	// Registered only once the call is otherwise valid, and after its own
	// references were checked against captures declared so far: a call can
	// reference an earlier capture, never one of its own.
	for _, c := range captures {
		if prev, ok := l.declared[c.Name]; ok {
			if prev.callName == spec.Name {
				return Call{}, fmt.Errorf("capture %q: declared twice by %s", c.Name, ref)
			}
			return Call{}, fmt.Errorf("capture %q: declared by call %q and by %s", c.Name, prev.callName, ref)
		}
		l.declared[c.Name] = capDecl{callName: spec.Name, continueOnFailure: spec.ContinueOnFailure}
	}

	return Call{
		Name: spec.Name,
		// Defaulted here rather than left to net/http's implicit GET, so a
		// loaded Call fully describes the request it will send: --dry-run
		// prints Method verbatim.
		Method:            cmp.Or(spec.Method, http.MethodGet),
		Path:              spec.Path,
		Body:              body,
		HasBody:           hasBody,
		Query:             spec.Query,
		Headers:           headers,
		Retry:             retryCfg,
		RetryWhen:         retryWhen,
		SuccessWhen:       successWhen,
		ContinueOnFailure: spec.ContinueOnFailure,
		Capture:           captures,
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
// execution time. Without this check, an invalid method like "GET EXTRA"
// loaded cleanly and failed only mid-run, after earlier calls had already
// mutated the cluster. Load-time validation exists to prevent exactly that.
// It reuses net/http's own token check instead of reimplementing RFC 7230's
// grammar, so load-time acceptance guarantees execution-time acceptance. An
// empty method defaults to GET, same as today.
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
// (textproto.CanonicalMIMEHeaderKey via http.Header.Set), so any
// case-spelling of a header in own always beats defaults, rather than
// leaving the winner to map-iteration order. Unlike Query, headers can't
// merge as plain map[string]string: two spellings of one header are distinct
// keys but not distinct headers.
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

// headerOverridden reports whether own sets a header canonically equal to
// key, making the inherited default value dead.
func headerOverridden(own map[string]string, key string) bool {
	for k := range own {
		if textproto.CanonicalMIMEHeaderKey(k) == textproto.CanonicalMIMEHeaderKey(key) {
			return true
		}
	}
	return false
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
	if v := rawNodeField(node, key); v != nil {
		return v.Value
	}
	return ""
}

// checkAllowedKeys rejects a mapping node with any key not in allowed, so a
// typo like succes-when fails to load instead of being silently ignored.
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

// rawNodeField returns the raw value node for key in a mapping node, or nil
// if absent, ahead of the allowed-key check and struct decode, so error
// messages can name the call even with invalid keys. Used for capture:
// (callSpec has no such field, see callSpec), as the shared walk beneath
// rawStringField, and to tell whether a call sets a key itself versus
// inheriting it from defaults: (checkRefs' inherited parameter).
func rawNodeField(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// mappingHasKey reports whether node itself declares key, as opposed to key
// reaching the merged spec only through defaults: layering. node is nil when
// the call omits the key altogether.
func mappingHasKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

// captureNameRe restricts a capture name to a jq-safe, referenceable
// identifier: unrestricted names could produce one nobody could ever write
// as ${name} again (e.g. a name containing "}" or a space).
var captureNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseCaptures parses the capture: mapping in document order, validating
// each name and compiling each expression. A nil node (no capture: key)
// yields a nil slice.
func parseCaptures(node *yaml.Node, ref string) ([]Capture, error) {
	if node == nil {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: capture: must be a mapping", ref)
	}

	captures := make([]Capture, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if !captureNameRe.MatchString(name) {
			return nil, fmt.Errorf("%s: capture %q: name must match %s", ref, name, captureNameRe.String())
		}
		expr := node.Content[i+1].Value
		compiled, err := compileCapture(name, expr)
		if err != nil {
			return nil, fmt.Errorf("%s: capture %q: %w", ref, name, err)
		}
		captures = append(captures, compiled)
	}
	return captures, nil
}

// rejectRefInKey rejects a "${" in a query or header key: only values can be
// substituted (checkRefs covers those), so a key containing one would ship
// an unresolved "${...}" to the server with no warning.
func rejectRefInKey(key, ref, field string) error {
	if strings.Contains(key, "${") {
		return fmt.Errorf("%s: %s key %q: references are not supported in query/header keys", ref, field, key)
	}
	return nil
}

// checkRefs validates every ${name} reference in s against declared, the
// capture names known so far, naming ref in any error. It can't distinguish
// an unknown name from a forward reference: a left-to-right walk sees them
// identically, and rejecting both also rules out capture cycles. inherited
// marks s as reached via defaults: layering, so the error names that
// explicitly, rather than pointing at a line inside defaults: with no
// explanation.
func checkRefs(s string, declared map[string]capDecl, ref string, inherited bool) error {
	source := ""
	if inherited {
		source = " (inherited from defaults:)"
	}
	_, err := scanTemplate(s, func(name string) (string, error) {
		decl, ok := declared[name]
		switch {
		case !ok:
			return "", fmt.Errorf("references ${%s}%s, which is not a capture declared by an earlier call", name, source)
		case decl.continueOnFailure:
			return "", fmt.Errorf(
				"references ${%s}%s, captured by call %q, which sets continue-on-failure: true"+
					" — a tolerated failure would leave ${%s} unset",
				name, source, decl.callName, name)
		}
		return "", nil
	})
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	return nil
}

// bodyRef augments ref with the resolved @file path when bodyArg names one,
// so a reference error inside the file points at that path. The file's own
// line numbers aren't tracked, but the path alone is usually enough.
func bodyRef(ref, bodyArg string) string {
	if len(bodyArg) < 2 || bodyArg[0] != '@' || bodyArg == "@-" {
		return ref
	}
	return fmt.Sprintf("%s (body file %q)", ref, bodyArg[1:])
}
