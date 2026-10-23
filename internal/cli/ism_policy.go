package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/logmanager-oss/opensearch-api/internal/osclient"
)

// ismServerFields are the policy keys the server owns. A PUT must not send them.
var ismServerFields = []string{"policy_id", "last_updated_time", "user", "schema_version"}

var errPolicyBody = errors.New("policy body")

type ismSetFlags struct {
	*ismFlags

	body     string
	targets  []string
	priority int
	force    bool
}

func newISMSetCommand(f *ismFlags) *cobra.Command {
	sf := &ismSetFlags{ismFlags: f}
	cmd := &cobra.Command{
		Use:           "set <id>",
		Short:         "Create or update an ISM policy",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runISMSet(cmd, sf, args[0])
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&sf.body, "body", "d", "",
		"policy body: a literal string, @file to read a file, or @- to read stdin")
	fl.StringArrayVar(&sf.targets, "target", nil,
		"index pattern for the policy's ism_template (repeatable); replaces the body's template")
	fl.IntVar(&sf.priority, "priority", 0, "ism_template priority; needs --target")
	fl.BoolVar(&sf.force, "force", false, "send the PUT even when the stored policy is equal")
	_ = cmd.MarkFlagRequired("body")
	return cmd
}

func runISMSet(cmd *cobra.Command, sf *ismSetFlags, id string) error {
	if err := validName("policy id", id); err != nil {
		return err
	}
	if cmd.Flags().Changed("priority") && len(sf.targets) == 0 {
		return errors.New("--priority needs --target")
	}
	raw, _, err := osclient.ReadBody(sf.body, cmd.InOrStdin())
	if err != nil {
		return err
	}
	policy, err := buildPolicy(raw, sf.targets, sf.priority)
	if err != nil {
		return err
	}
	body, err := marshalRaw(map[string]any{"policy": policy})
	if err != nil {
		return err
	}

	s, err := newISMSession(cmd, sf.ismFlags)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout(cmd.Context(), s.cfg.Timeout)
	defer cancel()

	var unchanged func([]byte) bool
	if !sf.force {
		unchanged = policyUnchanged(cmd.ErrOrStderr(), id, policy)
	}
	attempt, err := osclient.Upsert( //nolint:bodyclose // the attempt returns the response to streamBody
		s.client, s.cfg.Endpoint, osclient.RequestSpec{
			Method:  http.MethodPut,
			Path:    ismPoliciesPath + "/" + id,
			Body:    body,
			HasBody: true,
		}, unchanged)
	if err != nil {
		return err
	}
	return s.do(ctx, attempt)
}

// buildPolicy decodes the -d body, normalises its policy and applies --target.
func buildPolicy(raw []byte, targets []string, priority int) (map[string]any, error) {
	top, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errPolicyBody, err)
	}
	doc, ok := top.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: must be a JSON object", errPolicyBody)
	}
	p, ok := doc["policy"]
	if !ok {
		return nil, fmt.Errorf(`%w: top-level "policy" object is required`, errPolicyBody)
	}
	policy, err := normalizePolicy(p)
	if err != nil {
		return nil, err
	}
	if len(targets) > 0 {
		// Same dynamic types as a decoded GET body, or the unchanged comparison never matches.
		patterns := make([]any, len(targets))
		for i, t := range targets {
			patterns[i] = t
		}
		policy["ism_template"] = []any{map[string]any{
			"index_patterns": patterns, "priority": json.Number(strconv.Itoa(priority)),
		}}
	}
	return policy, nil
}

// normalizePolicy drops the server-owned fields so a policy read from GET can be written back.
func normalizePolicy(v any) (map[string]any, error) {
	policy, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(`%w: "policy" must be an object`, errPolicyBody)
	}
	for _, k := range ismServerFields {
		delete(policy, k)
	}
	tmpl, ok := policy["ism_template"]
	if !ok {
		return policy, nil
	}
	var entries []any
	switch t := tmpl.(type) {
	case map[string]any:
		entries = []any{t}
	case []any:
		entries = t
	default:
		return nil, fmt.Errorf(`%w: "ism_template" must be an object or an array of objects`, errPolicyBody)
	}
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(`%w: "ism_template" must be an object or an array of objects`, errPolicyBody)
		}
		delete(m, "last_updated_time")
	}
	policy["ism_template"] = entries
	return policy, nil
}

// policyUnchanged returns the Upsert hook: true when the stored policy equals
// want after normalising. It can only err toward "changed", never toward "unchanged".
func policyUnchanged(stderr io.Writer, id string, want map[string]any) func([]byte) bool {
	reported := false
	return func(body []byte) bool {
		top, err := decodeJSON(body)
		if err != nil {
			return false
		}
		doc, ok := top.(map[string]any)
		if !ok {
			return false
		}
		have, err := normalizePolicy(doc["policy"])
		if err != nil || !reflect.DeepEqual(have, want) {
			return false
		}
		if !reported {
			_, _ = fmt.Fprintf(stderr, "policy %s unchanged\n", id)
			reported = true
		}
		return true
	}
}

// decodeJSON decodes with UseNumber so integers past 2^53 and decimals keep
// their exact text through the comparison and the PUT.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the JSON value")
	}
	return out, nil
}

// marshalRaw marshals v without HTML escaping, so string values stay byte-exact.
func marshalRaw(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding policy: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
