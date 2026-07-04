// Package config resolves osapi runtime configuration from flags, environment
// variables and named profiles, and manages the on-disk profile file.
package config

import (
	"fmt"
	"strings"
	"time"
)

// BackoffStrategy selects how retry backoff grows between attempts.
type BackoffStrategy int

const (
	// Constant keeps the delay fixed at Initial.
	Constant BackoffStrategy = iota + 1
	// Linear grows the delay by Initial each attempt.
	Linear
	// Exponential doubles the delay each attempt.
	Exponential
)

func (b BackoffStrategy) String() string {
	switch b {
	case Constant:
		return "constant"
	case Linear:
		return "linear"
	case Exponential:
		return "exponential"
	default:
		return "unknown"
	}
}

// ParseBackoffStrategy parses a case-insensitive strategy name.
func ParseBackoffStrategy(s string) (BackoffStrategy, error) {
	switch strings.ToLower(s) {
	case "constant":
		return Constant, nil
	case "linear":
		return Linear, nil
	case "exponential":
		return Exponential, nil
	default:
		return 0, fmt.Errorf("unknown backoff strategy %q", s)
	}
}

// RetryConfig is the resolved retry behaviour for a request.
type RetryConfig struct {
	MaxAttempts    int // 0 = infinite
	Strategy       BackoffStrategy
	Initial        time.Duration
	Max            time.Duration
	Jitter         float64
	SuccessStatus  []int // empty => any 2xx
	TerminalStatus []int // always terminal (default {409})
	RetryStatus    []int // empty => retry all non-success/non-terminal
}

// Config is the fully resolved runtime configuration.
type Config struct {
	Endpoint   string
	Username   string
	Password   string
	CACertPath string
	Insecure   bool
	Profile    string
	Retry      RetryConfig
}

const redacted = "***"

// configAlias drops the String/GoString methods to avoid infinite recursion
// while formatting a redacted copy.
type configAlias Config

// String redacts the password so it never leaks through %v/%+v/%s formatting.
func (c Config) String() string {
	if c.Password != "" {
		c.Password = redacted
	}
	return fmt.Sprintf("%v", configAlias(c))
}

// GoString redacts the password so it never leaks through %#v formatting.
//
//nolint:gocritic // value receiver required to satisfy fmt.GoStringer on a Config value.
func (c Config) GoString() string {
	if c.Password != "" {
		c.Password = redacted
	}
	return fmt.Sprintf("%#v", configAlias(c))
}

const (
	defaultInitial = 2 * time.Second
	defaultMax     = 30 * time.Second
)

// Defaults returns the baseline configuration before any overrides.
func Defaults() Config {
	return Config{
		Retry: RetryConfig{
			MaxAttempts:    0,
			Strategy:       Linear,
			Initial:        defaultInitial,
			Max:            defaultMax,
			TerminalStatus: []int{409},
		},
	}
}
