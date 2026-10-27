// Package secret names and finds Secrets: values a Node's config refers to as
// "$NAME" but never contains, found in the environment of the process reading
// the config, else in the Group's Vault
// (docs/adr/0013-group-config-workspaces-and-named-secrets.md).
package secret

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Where a Secret's value came from.
const (
	SourceEnv   = "env"
	SourceVault = "vault"
	SourceNone  = "none"
)

// ErrUnresolved is a reference found neither in the environment nor in the
// Vault.
var ErrUnresolved = errors.New("secret: reference resolves nowhere")

var namePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ValidateName checks that name may name a Secret.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("secret: name %q must match [A-Z_][A-Z0-9_]*", name)
	}
	return nil
}

// ParseRef returns the Secret name a whole-value reference "$NAME" names.
// Anything else, a literal or a reference inside a longer string, is refused
// without echoing it, since it may be the secret itself.
func ParseRef(v string) (string, error) {
	name, ok := strings.CutPrefix(v, "$")
	if !ok || !namePattern.MatchString(name) {
		return "", errors.New(`secret: must be a "$NAME" reference to a Secret, NAME matching [A-Z_][A-Z0-9_]*, not a value`)
	}
	return name, nil
}

// Resolver finds Secrets for one Group. The Vault is opened at most once,
// and only when the environment lacks a name.
type Resolver struct {
	Group  string
	Getenv func(string) string
	// Vault loads the Group's stored Secrets; nil means there are none.
	Vault func() (map[string]string, error)

	stored map[string]string
	loaded bool
}

// Lookup finds name: the environment when set and non-empty, else the Vault.
func (r *Resolver) Lookup(name string) (value, source string, err error) {
	if v := r.Getenv(name); v != "" {
		return v, SourceEnv, nil
	}
	if !r.loaded && r.Vault != nil {
		if r.stored, err = r.Vault(); err != nil {
			return "", "", err
		}
		r.loaded = true
	}
	if v := r.stored[name]; v != "" {
		return v, SourceVault, nil
	}
	return "", SourceNone, nil
}

// Resolve is the value of the reference ref, or ErrUnresolved naming the
// reference and the Group.
func (r *Resolver) Resolve(ref string) (string, error) {
	name, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	v, source, err := r.Lookup(name)
	if err != nil {
		return "", err
	}
	if source == SourceNone {
		return "", fmt.Errorf("%w: $%s for Group %s is in neither the environment nor the Vault", ErrUnresolved, name, r.Group)
	}
	return v, nil
}
