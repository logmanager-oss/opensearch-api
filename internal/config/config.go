// Package config resolves osapi runtime configuration from flags, environment
// variables and env files.
package config

import (
	"fmt"
	"math"
	"strconv"
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
	MaxRetries int // number of retries; 0 = no retry (single attempt), <0 = unlimited
	Strategy   BackoffStrategy
	Initial    time.Duration
	Max        time.Duration
	Jitter     float64
	// AbortOn lists non-2xx status codes that stop retrying (abort), regardless
	// of RetryWhen/SuccessWhen — it applies only to non-2xx statuses and always
	// wins over the body predicates. Empty means retry every non-2xx response.
	// Any 2xx is always success.
	AbortOn []int
	// RetryWhen is a raw jq expression evaluated against the response body; a
	// match triggers a retry. Compiled and evaluated by the retry layer, not
	// internal/config.
	RetryWhen string
	// SuccessWhen is a raw jq expression evaluated against the response body; a
	// match short-circuits retrying as success. Compiled and evaluated by the
	// retry layer, not internal/config.
	SuccessWhen string
	// MaxBodyBuffer caps the bytes buffered from the response body for
	// RetryWhen/SuccessWhen evaluation; 0 means unlimited.
	MaxBodyBuffer int64
}

// Config is the fully resolved runtime configuration.
type Config struct {
	Endpoint   string
	Username   string
	Password   string
	CACertPath string
	Insecure   bool
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

// DefaultMaxBodyBuffer is the flag-default string for RetryConfig.MaxBodyBuffer.
// Defaults parses it, and the CLI layer registers it verbatim as the flag
// default, so the two representations can never drift apart.
const DefaultMaxBodyBuffer = "10MiB"

// Defaults returns the baseline configuration before any overrides.
func Defaults() Config {
	maxBodyBuffer, err := ParseSize(DefaultMaxBodyBuffer)
	if err != nil {
		panic(fmt.Sprintf("config: DefaultMaxBodyBuffer %q is not a valid size: %v", DefaultMaxBodyBuffer, err))
	}

	return Config{
		Retry: RetryConfig{
			MaxRetries:    0, // no retry by default; retrying is opt-in via --retry
			Strategy:      Linear,
			Initial:       defaultInitial,
			Max:           defaultMax,
			MaxBodyBuffer: maxBodyBuffer,
		},
	}
}

const (
	bytesPerKiB int64 = 1 << 10
	bytesPerMiB int64 = 1 << 20
	bytesPerGiB int64 = 1 << 30
)

const (
	sizeErrFormat      = "invalid size %q: want <integer>[B|KiB|MiB|GiB]"
	sizeRangeErrFormat = "invalid size %q: out of range"
)

// ParseSize parses a human-readable byte size: a bare integer is bytes, or an
// integer followed by a case-insensitive unit (B, KiB, MiB, GiB; KB/MB/GB are
// 1024-based aliases of KiB/MiB/GiB). Decimals and negative values are errors.
func ParseSize(s string) (int64, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf(sizeErrFormat, s)
	}

	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf(sizeRangeErrFormat, s)
	}

	var mult int64
	switch strings.ToUpper(s[i:]) {
	case "", "B":
		mult = 1
	case "KB", "KIB":
		mult = bytesPerKiB
	case "MB", "MIB":
		mult = bytesPerMiB
	case "GB", "GIB":
		mult = bytesPerGiB
	default:
		return 0, fmt.Errorf(sizeErrFormat, s)
	}

	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf(sizeRangeErrFormat, s)
	}

	return n * mult, nil
}

// FormatSize renders n bytes using the largest unit of ParseSize's grammar
// that divides it evenly, so a parsed size round-trips to its input form.
func FormatSize(n int64) string {
	switch {
	case n >= bytesPerGiB && n%bytesPerGiB == 0:
		return strconv.FormatInt(n/bytesPerGiB, 10) + "GiB"
	case n >= bytesPerMiB && n%bytesPerMiB == 0:
		return strconv.FormatInt(n/bytesPerMiB, 10) + "MiB"
	case n >= bytesPerKiB && n%bytesPerKiB == 0:
		return strconv.FormatInt(n/bytesPerKiB, 10) + "KiB"
	default:
		return strconv.FormatInt(n, 10) + "B"
	}
}
