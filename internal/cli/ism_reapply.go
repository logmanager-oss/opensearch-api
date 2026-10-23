package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/logmanager-oss/opensearch-api/internal/osclient"
)

// ismExplainSize is the explain page size: one page must cover every managed index.
const ismExplainSize = 10000

type ismReapplyFlags struct {
	*ismFlags

	target string
}

func newISMReapplyCommand(f *ismFlags) *cobra.Command {
	rf := &ismReapplyFlags{ismFlags: f}
	cmd := &cobra.Command{
		Use:   "reapply <id> [--target <index>]",
		Short: "Re-run a policy's current version on the indices it manages",
		Long: "List the indices managed by policy <id> through explain, then send change_policy " +
			"for exactly those. An index that matches --target but belongs to another policy is left alone.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runISMReapply(cmd, rf, args[0])
		},
	}
	cmd.Flags().StringVar(&rf.target, "target", "",
		"index names or patterns, comma-separated, passed through verbatim (default: all managed indices)")
	return cmd
}

func runISMReapply(cmd *cobra.Command, rf *ismReapplyFlags, id string) error {
	if err := validName("policy id", id); err != nil {
		return err
	}
	path := ismExplainPath
	if rf.target != "" {
		if err := validName("target", rf.target); err != nil {
			return err
		}
		path += "/" + rf.target
	}

	s, err := newISMSession(cmd, rf.ismFlags)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout(cmd.Context(), s.cfg.Timeout)
	defer cancel()

	explain, err := s.fetch(ctx, osclient.RequestSpec{
		Method: http.MethodGet,
		Path:   path,
		Query:  map[string]string{"size": strconv.Itoa(ismExplainSize)},
	})
	if err != nil {
		return err
	}
	indices, err := managedBy(explain, id)
	if err != nil {
		return err
	}
	if len(indices) == 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "no indices are managed by %s\n", id)
		return nil
	}

	body, err := marshalRaw(map[string]string{"policy_id": id})
	if err != nil {
		return err
	}
	return s.send(ctx, osclient.RequestSpec{
		Method:  http.MethodPost,
		Path:    ismChangePolicy + "/" + strings.Join(indices, ","),
		Body:    body,
		HasBody: true,
	})
}

// managedBy returns the sorted index names in an explain body whose policy_id is id.
// Values that are not objects, such as total_managed_indices, are skipped.
func managedBy(explain []byte, id string) ([]string, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(explain, &all); err != nil {
		return nil, fmt.Errorf("decoding explain response: %w", err)
	}
	var out []string
	for name, raw := range all {
		var entry struct {
			PolicyID string `json:"policy_id"`
		}
		if json.Unmarshal(raw, &entry) == nil && entry.PolicyID == id {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}
