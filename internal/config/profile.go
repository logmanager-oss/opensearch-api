package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ErrLoosePerms indicates the config file permissions are looser than 0600.
var ErrLoosePerms = errors.New("config file permissions are too loose")

// RetryDefaults mirrors RetryConfig for persistence. Scalar fields are pointers
// so an unset field ("nil") is distinguishable from a zero value and does not
// override lower-precedence configuration. Durations are stored as strings
// (e.g. "2s") parseable by time.ParseDuration for human-friendly YAML.
type RetryDefaults struct {
	MaxAttempts    *int     `yaml:"max_attempts,omitempty"`
	Strategy       *string  `yaml:"strategy,omitempty"`
	Initial        *string  `yaml:"initial,omitempty"`
	Max            *string  `yaml:"max,omitempty"`
	Jitter         *float64 `yaml:"jitter,omitempty"`
	SuccessStatus  []int    `yaml:"success_status,omitempty"`
	TerminalStatus []int    `yaml:"terminal_status,omitempty"`
	RetryStatus    []int    `yaml:"retry_status,omitempty"`
}

// Profile is a persisted named connection. Passwords are never stored.
type Profile struct {
	Name       string         `yaml:"name"`
	Endpoint   string         `yaml:"endpoint,omitempty"`
	Username   string         `yaml:"username,omitempty"`
	CACertPath string         `yaml:"ca_cert_path,omitempty"`
	Insecure   bool           `yaml:"insecure,omitempty"`
	Retry      *RetryDefaults `yaml:"retry,omitempty"`
}

// ProfileFile is the on-disk config file structure.
type ProfileFile struct {
	Profiles []Profile `yaml:"profiles"`
}

const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// LoadProfiles reads the profile file. A missing file yields an empty
// ProfileFile and no error.
func LoadProfiles(path string) (ProfileFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ProfileFile{}, nil
	}
	if err != nil {
		return ProfileFile{}, fmt.Errorf("reading profiles from %s: %w", path, err)
	}

	var pf ProfileFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return ProfileFile{}, fmt.Errorf("parsing profiles from %s: %w", path, err)
	}
	return pf, nil
}

// SaveProfiles writes the profile file, enforcing 0700 on the parent directory
// and 0600 on the file. Chmod is explicit so a pre-existing looser file/dir is
// tightened, not just newly created ones.
func SaveProfiles(path string, pf ProfileFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating config dir for %s: %w", path, err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("tightening permissions on %s: %w", dir, err)
	}

	data, err := yaml.Marshal(pf)
	if err != nil {
		return fmt.Errorf("marshaling profiles: %w", err)
	}

	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("writing profiles to %s: %w", path, err)
	}
	if err := os.Chmod(path, filePerm); err != nil {
		return fmt.Errorf("tightening permissions on %s: %w", path, err)
	}
	return nil
}

// FindProfile returns the named profile if present.
func FindProfile(pf ProfileFile, name string) (*Profile, bool) {
	for i := range pf.Profiles {
		if pf.Profiles[i].Name == name {
			return &pf.Profiles[i], true
		}
	}
	return nil, false
}

// CheckPerms returns ErrLoosePerms if the file is readable/writable beyond the
// owner (looser than 0600). A missing file is not an error.
func CheckPerms(path string) error {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking permissions of %s: %w", path, err)
	}
	if fi.Mode().Perm()&^os.FileMode(filePerm) != 0 {
		return fmt.Errorf("%s has mode %o: %w", path, fi.Mode().Perm(), ErrLoosePerms)
	}
	return nil
}

const (
	envOSAPIConfig = "OSAPI_CONFIG"
	defaultDir     = ".osapi"
	defaultFile    = "config.yaml"
)

// ConfigPath resolves the config file path: --config flag > OSAPI_CONFIG env >
// ~/.osapi/config.yaml.
func ConfigPath(flagVal string, env EnvLookup) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if v, ok := lookupEnv(env, envOSAPIConfig); ok {
		return v, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, defaultDir, defaultFile), nil
}
