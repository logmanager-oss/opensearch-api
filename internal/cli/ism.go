package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/logmanager-oss/opensearch-api/internal/config"
	"github.com/logmanager-oss/opensearch-api/internal/osclient"
	"github.com/logmanager-oss/opensearch-api/internal/retry"
)

const (
	ismPrefix         = "_plugins/_ism/"
	ismPoliciesPath   = ismPrefix + "policies"
	ismChangePolicy   = ismPrefix + "change_policy"
	ismExplainPath    = ismPrefix + "explain"
	ismInvalidNameSet = "/?#%"
)

// ismFlags holds the connection and retry flags shared by every ism verb.
type ismFlags struct {
	connFlags
	retryFlags
}

// newISMCommand builds the ism group and its two verbs.
func newISMCommand() *cobra.Command {
	f := &ismFlags{}
	ism := &cobra.Command{
		Use:   "ism",
		Short: "Manage Index State Management policies",
	}
	registerConnFlags(ism.PersistentFlags(), &f.connFlags)
	registerRetryFlags(ism, ism.PersistentFlags(), &f.retryFlags)
	ism.AddCommand(newISMSetCommand(f))
	ism.AddCommand(newISMReapplyCommand(f))
	return ism
}

// ismSession is what every verb needs after flag resolution.
type ismSession struct {
	cmd     *cobra.Command
	cfg     config.Config
	preds   predicates
	verbose bool
	client  *http.Client
}

// newISMSession resolves the flags and builds the client. Call it once per verb.
func newISMSession(cmd *cobra.Command, f *ismFlags) (*ismSession, error) {
	cfg, preds, err := resolveConfig(cmd, &f.connFlags, &f.retryFlags)
	if err != nil {
		return nil, err
	}
	client, err := newClient(cmd, &cfg)
	if err != nil {
		return nil, err
	}
	return &ismSession{cmd: cmd, cfg: cfg, preds: preds, verbose: f.verbose, client: client}, nil
}

// do runs attempt under the retry policy and streams the final body to stdout.
func (s *ismSession) do(ctx context.Context, attempt retry.Attempt) error {
	resp, doErr := doWithRetry(s.cmd, ctx, &s.cfg, s.preds, s.verbose, attempt) //nolint:bodyclose // streamBody closes it
	return streamBody(s.cmd, ctx, &s.cfg, resp, doErr)
}

// fetch runs spec and returns the body. On failure it writes the body to stdout.
// It ignores --retry-when/--success-when, which describe the final response.
//
//nolint:gocritic // hugeParam: RequestSpec is the documented value-typed public API.
func (s *ismSession) fetch(ctx context.Context, spec osclient.RequestSpec) ([]byte, error) {
	req, err := osclient.BuildRequest(s.cfg.Endpoint, spec)
	if err != nil {
		return nil, err
	}
	resp, doErr := doWithRetry(s.cmd, ctx, &s.cfg, predicates{}, s.verbose, osclient.Replay(s.client, req)) //nolint:bodyclose // closed below
	if resp == nil {
		return nil, timeoutError(ctx, s.cfg.Timeout, doErr)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if doErr != nil {
		_, _ = s.cmd.OutOrStdout().Write(body)
		return nil, timeoutError(ctx, s.cfg.Timeout, doErr)
	}
	if readErr != nil {
		return nil, fmt.Errorf("reading response body: %w", readErr)
	}
	return body, nil
}

// validName rejects an empty name, a dot segment, or one containing / ? # or %. It keeps ","
// legal, which url.PathEscape would escape.
func validName(kind, s string) error {
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("invalid %s %q: must be a name", kind, s)
	}
	if strings.ContainsAny(s, ismInvalidNameSet) {
		return fmt.Errorf("invalid %s %q: must not contain any of %s", kind, s, ismInvalidNameSet)
	}
	return nil
}
