package config

import (
	"fmt"
	"slices"
	"time"
)

// Field names identify flags that were explicitly set. The CLI layer wires
// these to cobra's flags.Changed so Resolve can apply flag > env > profile >
// default precedence per field.
const (
	FieldEndpoint       = "endpoint"
	FieldUsername       = "username"
	FieldPassword       = "password"
	FieldCACert         = "ca-cert"
	FieldInsecure       = "insecure"
	FieldMaxAttempts    = "max-attempts"
	FieldBackoff        = "backoff"
	FieldBackoffInitial = "backoff-initial"
	FieldBackoffMax     = "backoff-max"
	FieldBackoffJitter  = "backoff-jitter"
	FieldTerminalStatus = "terminal-status"
	FieldRetryStatus    = "retry-status"
	FieldSuccessStatus  = "success-status"
	FieldExpectEmpty    = "expect-empty"
)

// Sources bundles the inputs to Resolve, decoupled from cobra:
//   - Flags carries the flag-provided values.
//   - Changed reports whether a given field (see Field* constants) was set on
//     the command line; nil means nothing was set.
//   - Env is the environment lookup, consulted only when UseEnv is true.
//   - Profile is the selected named profile, or nil.
type Sources struct {
	Flags   Config
	Changed func(field string) bool
	Env     EnvLookup
	UseEnv  bool
	Profile *Profile
}

func (s *Sources) changed(field string) bool {
	return s.Changed != nil && s.Changed(field)
}

// Resolve merges the sources into a final Config using per-field precedence
// explicit flag > env (when UseEnv) > profile > default. Passwords are resolved
// from flag/env only, never from a profile.
//
//nolint:gocritic // Sources is the documented value-typed public API.
func Resolve(s Sources) (Config, error) {
	cfg := Defaults()

	if s.Profile != nil {
		if err := applyProfile(&cfg, s.Profile); err != nil {
			return Config{}, err
		}
	}
	if s.UseEnv {
		applyEnv(&cfg, s.Env)
	}
	applyFlags(&cfg, &s)
	resolvePasswordSource(&cfg, &s)

	return cfg, nil
}

func applyProfile(cfg *Config, p *Profile) error {
	if p.Endpoint != "" {
		cfg.Endpoint = p.Endpoint
	}
	if p.Username != "" {
		cfg.Username = p.Username
	}
	if p.CACertPath != "" {
		cfg.CACertPath = p.CACertPath
	}
	if p.Insecure {
		cfg.Insecure = true
	}
	return applyRetryDefaults(&cfg.Retry, p.Retry)
}

func applyRetryDefaults(r *RetryConfig, d *RetryDefaults) error {
	if d == nil {
		return nil
	}
	if d.MaxAttempts != nil {
		r.MaxAttempts = *d.MaxAttempts
	}
	if d.Strategy != nil {
		parsed, err := ParseBackoffStrategy(*d.Strategy)
		if err != nil {
			return fmt.Errorf("profile retry strategy: %w", err)
		}
		r.Strategy = parsed
	}
	if err := applyDuration(&r.Initial, d.Initial, "initial"); err != nil {
		return err
	}
	if err := applyDuration(&r.Max, d.Max, "max"); err != nil {
		return err
	}
	if d.Jitter != nil {
		r.Jitter = *d.Jitter
	}
	if d.SuccessStatus != nil {
		r.SuccessStatus = slices.Clone(d.SuccessStatus)
	}
	if d.TerminalStatus != nil {
		r.TerminalStatus = slices.Clone(d.TerminalStatus)
	}
	if d.RetryStatus != nil {
		r.RetryStatus = slices.Clone(d.RetryStatus)
	}
	if d.ExpectEmpty != nil {
		r.ExpectEmpty = *d.ExpectEmpty
	}
	return nil
}

func applyDuration(dst *time.Duration, val *string, name string) error {
	if val == nil {
		return nil
	}
	d, err := time.ParseDuration(*val)
	if err != nil {
		return fmt.Errorf("profile retry %s duration %q: %w", name, *val, err)
	}
	*dst = d
	return nil
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
	if s.changed(FieldMaxAttempts) {
		cfg.Retry.MaxAttempts = f.Retry.MaxAttempts
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
	if s.changed(FieldTerminalStatus) {
		cfg.Retry.TerminalStatus = slices.Clone(f.Retry.TerminalStatus)
	}
	if s.changed(FieldRetryStatus) {
		cfg.Retry.RetryStatus = slices.Clone(f.Retry.RetryStatus)
	}
	if s.changed(FieldSuccessStatus) {
		cfg.Retry.SuccessStatus = slices.Clone(f.Retry.SuccessStatus)
	}
	if s.changed(FieldExpectEmpty) {
		cfg.Retry.ExpectEmpty = f.Retry.ExpectEmpty
	}
}

// resolvePasswordSource resolves the password from env then flag; a profile
// never supplies a password.
func resolvePasswordSource(cfg *Config, s *Sources) {
	if s.UseEnv {
		if v, ok := envPassword(s.Env); ok {
			cfg.Password = v
		}
	}
	if s.changed(FieldPassword) {
		cfg.Password = s.Flags.Password
	}
}
