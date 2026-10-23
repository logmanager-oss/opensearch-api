package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"

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
		unchanged, err = policyUnchanged(cmd.ErrOrStderr(), id, policy)
		if err != nil {
			return err
		}
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
func buildPolicy(raw []byte, targets []string, priority int) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("%w: %w", errPolicyBody, err)
	}
	p, ok := top["policy"]
	if !ok {
		return nil, fmt.Errorf(`%w: top-level "policy" object is required`, errPolicyBody)
	}
	policy, err := normalizePolicy(p)
	if err != nil {
		return nil, err
	}
	if len(targets) > 0 {
		if err := withTemplate(policy, targets, priority); err != nil {
			return nil, err
		}
	}
	return policy, nil
}

// normalizePolicy drops the server-owned fields so a policy read from GET can be written back.
func normalizePolicy(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &policy); err != nil || policy == nil {
		return nil, fmt.Errorf(`%w: "policy" must be an object`, errPolicyBody)
	}
	for _, k := range ismServerFields {
		delete(policy, k)
	}
	tmpl, ok := policy["ism_template"]
	if !ok {
		return policy, nil
	}
	var entries []map[string]json.RawMessage
	if t := bytes.TrimSpace(tmpl); bytes.Equal(t, []byte("null")) {
		return nil, fmt.Errorf(`%w: "ism_template" must be an object or an array of objects, not null`, errPolicyBody)
	} else if len(t) > 0 && t[0] == '{' {
		var one map[string]json.RawMessage
		if err := json.Unmarshal(tmpl, &one); err != nil {
			return nil, fmt.Errorf(`%w: "ism_template": %w`, errPolicyBody, err)
		}
		entries = []map[string]json.RawMessage{one}
	} else if err := json.Unmarshal(tmpl, &entries); err != nil {
		return nil, fmt.Errorf(`%w: "ism_template" must be an object or an array of objects: %w`, errPolicyBody, err)
	}
	for _, e := range entries {
		delete(e, "last_updated_time")
	}
	out, err := marshalRaw(entries)
	if err != nil {
		return nil, err
	}
	policy["ism_template"] = out
	return policy, nil
}

// withTemplate replaces the policy's ism_template with one entry for targets.
func withTemplate(policy map[string]json.RawMessage, targets []string, priority int) error {
	out, err := marshalRaw([]map[string]any{{"index_patterns": targets, "priority": priority}})
	if err != nil {
		return err
	}
	policy["ism_template"] = out
	return nil
}

// policyUnchanged returns the Upsert hook: true when the stored policy equals
// want after normalising. It can only err toward "changed", never toward "unchanged".
func policyUnchanged(stderr io.Writer, id string, want map[string]json.RawMessage) (func([]byte) bool, error) {
	wantAny, err := toAny(want)
	if err != nil {
		return nil, err
	}
	reported := false
	return func(body []byte) bool {
		var doc map[string]json.RawMessage
		if json.Unmarshal(body, &doc) != nil {
			return false
		}
		have, err := normalizePolicy(doc["policy"])
		if err != nil {
			return false
		}
		haveAny, err := toAny(have)
		if err != nil || !reflect.DeepEqual(haveAny, wantAny) {
			return false
		}
		if !reported {
			_, _ = fmt.Fprintf(stderr, "policy %s unchanged\n", id)
			reported = true
		}
		return true
	}, nil
}

// toAny re-decodes m so reflect.DeepEqual compares values, not raw bytes.
// Numbers stay json.Number: float64 would equate integers past 2^53.
func toAny(m map[string]json.RawMessage) (any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding policy: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding policy: %w", err)
	}
	return out, nil
}

// marshalRaw marshals v without HTML escaping, so raw values stay byte-exact.
func marshalRaw(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding policy: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
