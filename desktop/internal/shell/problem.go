package shell

import (
	"errors"
	"fmt"

	"defios.com/internal/node"
	"defios.com/internal/vault"
)

// Problem is an error as the app shows it: a Code the frontend can branch
// on and a Message for a person. Codes come from internal/node's typed
// errors, never from an error's text.
type Problem struct {
	Code    string
	Message string
}

// problemOf maps an internal/node error onto what the app shows.
func problemOf(err error) *Problem {
	var (
		noURL *node.NoHeadscaleURLError
		ident *node.IdentityError
		left  *vault.LeftoverError
	)
	switch {
	case errors.Is(err, vault.ErrBadPassphrase):
		return &Problem{"bad-passphrase", "That passphrase does not unlock this device."}
	case errors.Is(err, node.ErrServedElsewhere):
		return &Problem{"served-elsewhere", "Another program on this computer is running DeFiOS for this device."}
	case errors.Is(err, node.ErrUnknownGroup):
		return &Problem{"unknown-group", "This device is not in that Group."}
	case errors.Is(err, node.ErrNoNode):
		return &Problem{"no-node", "There is no DeFiOS device set up on this computer yet."}
	case errors.Is(err, node.ErrLocked):
		return &Problem{"locked", "Unlock this device first."}
	case errors.Is(err, node.ErrUnlocked):
		return &Problem{"unlocked", "This device is already unlocked."}
	case errors.Is(err, vault.ErrBusy):
		return &Problem{"vault-busy", "A passphrase change is under way; try again once it finishes."}
	case errors.As(err, &left):
		return &Problem{"vault-leftover", fmt.Sprintf("An interrupted passphrase change left %s behind; run `defios passphrase change` to finish it.", left.Dir)}
	case errors.As(err, &noURL):
		return &Problem{"no-headscale-url", fmt.Sprintf("This Group has no control server: set headscale_url in %s.", noURL.ConfigFile)}
	case errors.As(err, &ident):
		return &Problem{"identity", "The Vault opened, but its keys do not match this device's identity."}
	}
	return &Problem{"error", err.Error()}
}

// problemOrNil is problemOf(err), or nil for no error.
func problemOrNil(err error) *Problem {
	if err == nil {
		return nil
	}
	return problemOf(err)
}

// appProblem is an error of the app's own, from the OS: the keychain,
// start at login or the preferences file.
func appProblem(code string, err error) *Problem {
	return &Problem{code, err.Error()}
}
