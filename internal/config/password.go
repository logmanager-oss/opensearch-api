package config

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"
)

// ErrNoPassword indicates a username was set but no password could be resolved
// and no interactive terminal is available to prompt on.
var ErrNoPassword = errors.New("no password available")

// ResolvePassword returns the password to use, prompting only as a last resort.
// Precedence: an already-resolved cfg.Password wins; an empty username means no
// authentication (no prompt); otherwise on a TTY the injected prompt is used,
// and on a non-TTY an error is returned. The prompt is injected to keep this
// function unit-testable.
//
//nolint:gocritic // Config is the documented value-typed public API.
func ResolvePassword(cfg Config, prompt func() (string, error), isTTY bool) (string, error) {
	if cfg.Password != "" {
		return cfg.Password, nil
	}
	if cfg.Username == "" {
		return "", nil
	}
	if !isTTY || prompt == nil {
		return "", fmt.Errorf("username %q set but no password; set OPENSEARCH_PASSWORD or pass --password: %w", cfg.Username, ErrNoPassword)
	}

	pass, err := prompt()
	if err != nil {
		return "", fmt.Errorf("reading password prompt: %w", err)
	}
	return pass, nil
}

// TerminalPrompt returns a prompt function that reads a masked password from
// the terminal. It is the real-use wrapper around term.ReadPassword; inject it
// into ResolvePassword at the call site.
func TerminalPrompt(user string) func() (string, error) {
	return func() (string, error) {
		fmt.Fprintf(os.Stderr, "Password for %q: ", user)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("reading masked password: %w", err)
		}
		return string(b), nil
	}
}
