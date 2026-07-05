package config

import (
	"bytes"
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

// LoadEnvFile reads a dotenv-style file into a key/value map without touching
// the process environment. Parsing is delegated to godotenv, but we do not
// surface godotenv's raw parse error, which echoes the offending line, so a
// secret on a malformed line is never leaked (e.g. into CI logs). A missing or
// malformed file is an error.
func LoadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading env file %s: %w", path, err)
	}

	vars, err := godotenv.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parsing env file %s: malformed contents (check for lines without '=' or unterminated quotes)", path)
	}
	return vars, nil
}

// LayeredEnv returns an EnvLookup where a non-empty fileVars[key] wins,
// otherwise it falls back to base (nil-safe). This realizes env file > process
// environment. An empty file value is treated as unset and falls through to
// base, consistent with the empty-as-unset rule.
func LayeredEnv(fileVars map[string]string, base EnvLookup) EnvLookup {
	return func(key string) (string, bool) {
		if v, ok := fileVars[key]; ok && v != "" {
			return v, true
		}
		if base == nil {
			return "", false
		}
		return base(key)
	}
}
