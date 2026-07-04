package config

// EnvLookup looks up an environment variable, reporting whether it was set.
// It mirrors os.LookupEnv and is injectable for testing. A nil EnvLookup is
// treated as "nothing set".
type EnvLookup func(string) (string, bool)

// Standard OpenSearch environment variables for connection and credentials.
const (
	envOpenSearchURL      = "OPENSEARCH_URL"
	envOpenSearchUsername = "OPENSEARCH_USERNAME"
	envOpenSearchPassword = "OPENSEARCH_PASSWORD"
)

// lookupEnv reads a single variable, staying nil-safe and treating an
// exported-but-empty value as unset.
func lookupEnv(env EnvLookup, key string) (string, bool) {
	if env == nil {
		return "", false
	}
	if v, ok := env(key); ok && v != "" {
		return v, true
	}
	return "", false
}

func envEndpoint(env EnvLookup) (string, bool) {
	return lookupEnv(env, envOpenSearchURL)
}

func envUsername(env EnvLookup) (string, bool) {
	return lookupEnv(env, envOpenSearchUsername)
}

func envPassword(env EnvLookup) (string, bool) {
	return lookupEnv(env, envOpenSearchPassword)
}
