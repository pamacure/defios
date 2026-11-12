package shell

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"defios.com/internal/backup"
	"defios.com/internal/group"
	"defios.com/internal/node"
	"defios.com/internal/serve"
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
		inv   *node.InvalidError

		notReached *node.NotReachedError
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
	case errors.Is(err, node.ErrNoPassphrase):
		return &Problem{"no-passphrase", "Enter this device's passphrase."}
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
	case errors.Is(err, node.ErrNodeExists):
		return &Problem{"node-exists", "DeFiOS is already set up on this computer."}
	case errors.Is(err, vault.ErrEmptyPassphrase):
		return &Problem{"empty-passphrase", "Enter a passphrase."}
	case errors.Is(err, node.ErrGroupExists):
		return &Problem{"group-exists", "This device already has a Group by that name."}
	case errors.As(err, &inv):
		return &Problem{"invalid", sentence(inv.Error())}
	case errors.As(err, &notReached):
		return &Problem{"not-reached", "The proposal did not reach every device, so it was withdrawn. Check they are online, then try again."}
	case errors.Is(err, serve.ErrRejected):
		return &Problem{"rejected", "Another device refused the proposal. It may already be in a Group, or hold another proposal."}
	case errors.Is(err, serve.ErrTailnetNotUp):
		return &Problem{"tailnet-down", "This device is not connected to the Group's tailnet yet."}
	case errors.Is(err, group.ErrOneGroup):
		return &Problem{"one-group", "This Group already has a proposal."}
	case errors.Is(err, node.ErrNotServed):
		return &Problem{"not-served", "This Group is not online on this device."}
	case errors.As(err, &ident):
		return &Problem{"identity", "The Vault opened, but its keys do not match this device's identity."}
	}
	return &Problem{"error", err.Error()}
}

// backupProblem is problemOf for restoring a Backup file, whose
// passphrase is the Backup's and whose missing file is the one chosen.
func backupProblem(err error) *Problem {
	var (
		notEmpty *node.ConfigNotEmptyError
		notFile  *node.NotAFileError
	)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vault.ErrBadPassphrase):
		return &Problem{"bad-passphrase", "That passphrase does not open this Backup."}
	case errors.Is(err, backup.ErrUnknownFormat):
		return &Problem{"unknown-backup", "This file is not a Backup this version of DeFiOS can read."}
	case errors.As(err, &notEmpty):
		return &Problem{"config-not-empty", fmt.Sprintf("A Backup restores only into an empty folder, and %s is not empty.", notEmpty.Dir)}
	case errors.As(err, &notFile):
		return &Problem{"not-a-file", fmt.Sprintf("%s is not a file.", notFile.Path)}
	case errors.Is(err, fs.ErrNotExist):
		return &Problem{"no-file", "That file is not there any more."}
	}
	return problemOf(err)
}

// sentence punctuates an InvalidError's message, which internal/node
// writes for a person, as a sentence: capitalized, ending in a full stop.
func sentence(msg string) string {
	if msg == "" {
		return msg
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
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
