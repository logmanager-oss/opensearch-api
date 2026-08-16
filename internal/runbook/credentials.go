package runbook

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/logmanager-oss/opensearch-api/internal/config"
)

// redactedSecret replaces Password in Credentials.String/GoString, the same
// pattern osclient.Options.String uses for its own Password field.
const redactedSecret = "***"

// credentialsAllowedKeys are the keys accepted inside a defaults:
// credentials: block.
var credentialsAllowedKeys = map[string]bool{"username": true, "password": true}

// Credentials holds a runbook's defaults: credentials: block as loaded:
// literal values or ${secret:NAME} references. Resolve produces the values
// a request uses.
// No yaml tags: parseCredentials reads the block node by node rather than
// decoding into this struct, so a tag-mismatch error can never quote a
// password.
type Credentials struct {
	Username string
	Password string
}

// credentialsAlias drops String/GoString to avoid infinite recursion while
// formatting a redacted copy.
type credentialsAlias Credentials

// String redacts Password so it never leaks through %v/%+v/%s formatting.
func (c Credentials) String() string {
	if c.Password != "" {
		c.Password = redactedSecret
	}
	return fmt.Sprintf("%v", credentialsAlias(c))
}

// GoString redacts Password so it never leaks through %#v formatting.
func (c Credentials) GoString() string {
	if c.Password != "" {
		c.Password = redactedSecret
	}
	return fmt.Sprintf("%#v", credentialsAlias(c))
}

// parseCredentials validates the defaults: credentials: mapping: shape,
// required keys, and ${secret:NAME} syntax. A nil node (no credentials:
// key) yields a nil *Credentials.
func parseCredentials(node *yaml.Node, ref string) (*Credentials, error) {
	if node == nil {
		return nil, nil
	}

	credRef := ref + ": credentials"
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: must be a mapping, got %s", credRef, nodeKindName(node.Kind))
	}
	if err := checkAllowedKeys(node, credentialsAllowedKeys, credRef); err != nil {
		return nil, err
	}

	username, err := credentialValue(node, credRef, "username")
	if err != nil {
		return nil, err
	}
	password, err := credentialValue(node, credRef, "password")
	if err != nil {
		return nil, err
	}
	if err := checkSecretRefs(username, "username"); err != nil {
		return nil, fmt.Errorf("%s: %w", credRef, err)
	}
	if err := checkSecretRefs(password, "password"); err != nil {
		return nil, fmt.Errorf("%s: %w", credRef, err)
	}
	return &Credentials{Username: username, Password: password}, nil
}

// Resolve returns c with ${secret:NAME} references in Username and
// Password replaced by lookup's value, leaving literals unchanged. Only
// Resolve touches the environment: Load never does.
func (c *Credentials) Resolve(lookup config.EnvLookup) (Credentials, error) {
	username, err := resolveSecretRefs(c.Username, "username", lookup)
	if err != nil {
		return Credentials{}, err
	}
	password, err := resolveSecretRefs(c.Password, "password", lookup)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Username: username, Password: password}, nil
}

// credentialValue reads one required credential scalar straight from the
// block. It bypasses node.Decode on purpose: yaml.v3 quotes the offending
// scalar in a tag-mismatch error, so `password: !!int hunter2` would print
// the password. It also separates an absent key from an empty one, so an
// author who wrote an empty password is not sent looking for a missing key.
func credentialValue(node *yaml.Node, ref, key string) (string, error) {
	field := rawNodeField(node, key)
	switch {
	case field == nil:
		return "", fmt.Errorf("%s: missing required key %q", ref, key)
	case field.Kind != yaml.ScalarNode:
		return "", fmt.Errorf("%s: %q must be a string, got %s", ref, key, nodeKindName(field.Kind))
	case field.Value == "":
		return "", fmt.Errorf("%s: %q must not be empty", ref, key)
	}
	return field.Value, nil
}

// checkSecretRefs validates the ${secret:NAME} references in s without
// reading the environment, so Load can reject a malformed reference before
// any secret is looked up. It mirrors the checkRefs/substitute split the
// capture machinery already uses, rather than keying the two modes off a
// sentinel lookup.
func checkSecretRefs(s, field string) error {
	if _, err := scanTemplate(s, func(ref string) (string, error) {
		_, err := secretName(ref)
		return "", err
	}); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// resolveSecretRefs resolves ${secret:NAME} references in s, using field
// ("username" or "password") for error messages. Unlike os.LookupEnv, an
// empty variable counts as unset, and a nil lookup means nothing is set.
func resolveSecretRefs(s, field string, lookup config.EnvLookup) (string, error) {
	out, err := scanTemplate(s, func(ref string) (string, error) {
		name, err := secretName(ref)
		if err != nil {
			return "", err
		}
		var val string
		var ok bool
		if lookup != nil {
			val, ok = lookup(name)
		}
		if !ok || val == "" {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		return val, nil
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return out, nil
}

// secretName strips the reserved secret: prefix from a ${...} reference and
// checks the remaining name, which shares captureNameRe so a reference stays
// writable as ${secret:NAME}.
func secretName(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "secret:")
	if !ok || !captureNameRe.MatchString(name) {
		return "", errors.New("only ${secret:NAME} references are supported in credentials")
	}
	return name, nil
}
