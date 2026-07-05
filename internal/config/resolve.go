package config

import (
	"slices"
)

// Field names identify flags that were explicitly set. The CLI layer wires
// these to cobra's flags.Changed so Resolve can apply flag > env > default
// precedence per field.
const (
	FieldEndpoint       = "endpoint"
	FieldUsername       = "username"
	FieldPassword       = "password"
	FieldCACert         = "ca-cert"
	FieldInsecure       = "insecure"
	FieldRetry          = "retry"
	FieldBackoff        = "backoff"
	FieldBackoffInitial = "backoff-initial"
	FieldBackoffMax     = "backoff-max"
	FieldBackoffJitter  = "backoff-jitter"
	FieldAbortOn        = "abort-on"
)

// Sources bundles the inputs to Resolve, decoupled from cobra:
//   - Flags carries the flag-provided values.
//   - Changed reports whether a given field (see Field* constants) was set on
//     the command line; nil means nothing was set.
//   - Env is the environment lookup, always consulted. Layer an env file over
//     the process environment with LayeredEnv.
type Sources struct {
	Flags   Config
	Changed func(field string) bool
	Env     EnvLookup
}

func (s *Sources) changed(field string) bool {
	return s.Changed != nil && s.Changed(field)
}

// Resolve merges the sources into a final Config using per-field precedence
// explicit flag > env > default.
//
//nolint:gocritic // Sources is the documented value-typed public API.
func Resolve(s Sources) (Config, error) {
	cfg := Defaults()

	applyEnv(&cfg, s.Env)
	applyFlags(&cfg, &s)
	resolvePasswordSource(&cfg, &s)

	return cfg, nil
}

func applyEnv(cfg *Config, env EnvLookup) {
	if v, ok := envEndpoint(env); ok {
		cfg.Endpoint = v
	}
	if v, ok := envUsername(env); ok {
		cfg.Username = v
	}
}

func applyFlags(cfg *Config, s *Sources) {
	f := s.Flags
	if s.changed(FieldEndpoint) {
		cfg.Endpoint = f.Endpoint
	}
	if s.changed(FieldUsername) {
		cfg.Username = f.Username
	}
	if s.changed(FieldCACert) {
		cfg.CACertPath = f.CACertPath
	}
	if s.changed(FieldInsecure) {
		cfg.Insecure = f.Insecure
	}
	if s.changed(FieldRetry) {
		cfg.Retry.MaxRetries = f.Retry.MaxRetries
	}
	if s.changed(FieldBackoff) {
		cfg.Retry.Strategy = f.Retry.Strategy
	}
	if s.changed(FieldBackoffInitial) {
		cfg.Retry.Initial = f.Retry.Initial
	}
	if s.changed(FieldBackoffMax) {
		cfg.Retry.Max = f.Retry.Max
	}
	if s.changed(FieldBackoffJitter) {
		cfg.Retry.Jitter = f.Retry.Jitter
	}
	if s.changed(FieldAbortOn) {
		cfg.Retry.AbortOn = slices.Clone(f.Retry.AbortOn)
	}
}

// resolvePasswordSource resolves the password from env then flag.
func resolvePasswordSource(cfg *Config, s *Sources) {
	if v, ok := envPassword(s.Env); ok {
		cfg.Password = v
	}
	if s.changed(FieldPassword) {
		cfg.Password = s.Flags.Password
	}
}
