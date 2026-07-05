// Command osapi is a general-purpose CLI for talking to OpenSearch REST
// endpoints with configurable retry.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/logmanager-oss/opensearch-api/internal/cli"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx)
	stop()
	os.Exit(code)
}

// run executes the root command against ctx and maps the result to an exit code.
func run(ctx context.Context) int {
	root := cli.NewRootCommand(version)
	err := root.ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return exitCode(err)
}

// exitCode maps a command error to a process exit code: 0 on success, 130 when
// the context was canceled (e.g. Ctrl-C), 1 otherwise.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return 130
	default:
		return 1
	}
}
