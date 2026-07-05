package cli

import (
	"github.com/spf13/cobra"

	"github.com/logmanager-oss/opensearch-api/internal/apispec"
)

// standardMethods is the fallback verb list offered for --method when the
// current --path does not exactly match a known template.
var standardMethods = []string{"GET", "POST", "PUT", "DELETE", "HEAD", "PATCH"}

// registerCompletion wires shell completion for --path and --method, driven by
// the OpenSearch API spec in apispec.
func registerCompletion(root *cobra.Command, qf *requestFlags) {
	_ = root.RegisterFlagCompletionFunc("path",
		func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			// Only filter by method when the user set -X; the default "GET"
			// would otherwise wrongly hide POST-only paths.
			method := ""
			if cmd.Flags().Changed("method") {
				method = qf.method
			}
			return apispec.Suggest(toComplete, method),
				cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp
		})

	_ = root.RegisterFlagCompletionFunc("method",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			methods := apispec.MethodsFor(qf.path)
			if len(methods) == 0 {
				methods = standardMethods
			}
			return methods, cobra.ShellCompDirectiveNoFileComp
		})
}
