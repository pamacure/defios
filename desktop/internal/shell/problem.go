package shell

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"defios.com/internal/backup"
	"defios.com/internal/chainfill"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/node"
	"defios.com/internal/outbox"
	"defios.com/internal/raftx"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/vault"
)

// Problem is an error as the app shows it: a Code the frontend can branch
// on, a Message for a person and, apart from it, a Detail: the error as a
// library words it, for whoever wants it, never part of the Message. Codes
// come from internal/node's typed errors, never from an error's text.
type Problem struct {
	Code    string
	Message string
	Detail  string
}

// withDetail is a Problem whose Detail is err's text.
func withDetail(code, msg string, err error) *Problem {
	return &Problem{Code: code, Message: msg, Detail: err.Error()}
}

// problemOf maps an internal/node error onto what the app shows.
func problemOf(err error) *Problem {
	var (
		noURL *node.NoHeadscaleURLError
		ident *node.IdentityError
		left  *vault.LeftoverError
		inv   *node.InvalidError

		notReached *node.NotReachedError
		taken      *node.LiveryTakenError
	)
	switch {
	case errors.As(err, &taken):
		return &Problem{Code: "livery-taken", Message: fmt.Sprintf("%s is already another party's Livery. Clear it there first, or pick another.", taken.Livery)}
	case errors.Is(err, node.ErrNoFreeLivery):
		return &Problem{Code: "no-free-livery", Message: "Every Livery is taken. Clear one first."}
	case errors.Is(err, vault.ErrBadPassphrase):
		return &Problem{Code: "bad-passphrase", Message: "That passphrase does not unlock this device."}
	case errors.Is(err, node.ErrServedElsewhere):
		return &Problem{Code: "served-elsewhere", Message: "Another program on this computer is running DeFiOS for this device."}
	case errors.Is(err, node.ErrUnknownGroup):
		return &Problem{Code: "unknown-group", Message: "This device is not in that Group."}
	case errors.Is(err, node.ErrNoNode):
		return &Problem{Code: "no-node", Message: "There is no DeFiOS device set up on this computer yet."}
	case errors.Is(err, node.ErrNoPassphrase):
		return &Problem{Code: "no-passphrase", Message: "Enter this device's passphrase."}
	case errors.Is(err, node.ErrLocked):
		return &Problem{Code: "locked", Message: "Unlock this device first."}
	case errors.Is(err, node.ErrUnlocked):
		return &Problem{Code: "unlocked", Message: "This device is already unlocked."}
	case errors.Is(err, vault.ErrBusy):
		return &Problem{Code: "vault-busy", Message: "A passphrase change is under way; try again once it finishes."}
	case errors.As(err, &left):
		return &Problem{Code: "vault-leftover", Message: fmt.Sprintf("An interrupted passphrase change left %s behind; run `defios passphrase change` to finish it.", left.Dir)}
	case errors.As(err, &noURL):
		return &Problem{Code: "no-headscale-url", Message: fmt.Sprintf("This Group has no control server: set headscale_url in %s.", noURL.ConfigFile)}
	case errors.Is(err, node.ErrNodeExists):
		return &Problem{Code: "node-exists", Message: "DeFiOS is already set up on this computer."}
	case errors.Is(err, vault.ErrEmptyPassphrase):
		return &Problem{Code: "empty-passphrase", Message: "Enter a passphrase."}
	case errors.Is(err, node.ErrGroupExists):
		return &Problem{Code: "group-exists", Message: "This device already has a Group by that name."}
	case errors.As(err, &inv):
		return &Problem{Code: "invalid", Message: sentence(inv.Error())}
	case errors.As(err, &notReached):
		return &Problem{Code: "not-reached", Message: "The proposal did not reach every device, so it was withdrawn. Check they are online, then try again."}
	case errors.Is(err, serve.ErrRejected):
		return &Problem{Code: "rejected", Message: "Another device refused the proposal. It may already be in a Group, or hold another proposal."}
	case errors.Is(err, serve.ErrTailnetNotUp):
		return &Problem{Code: "tailnet-down", Message: "This device is not connected to the Group's tailnet yet."}
	case errors.Is(err, group.ErrOneGroup):
		return &Problem{Code: "one-group", Message: "This Group already has a proposal."}
	case errors.Is(err, node.ErrNotServed):
		return &Problem{Code: "not-served", Message: "This Group is not online on this device."}
	case errors.As(err, &ident):
		return &Problem{Code: "identity", Message: "The Vault opened, but its keys do not match this device's identity."}
	}
	if p := signProblem(err); p != nil {
		return p
	}
	if p := agentsProblem(err); p != nil {
		return p
	}
	if p := triggersProblem(err); p != nil {
		return p
	}
	if p := pluginsProblem(err); p != nil {
		return p
	}
	return talkProblem(err)
}

// signProblem maps the Sign Request errors: proposing, filling, answering
// and broadcasting; nil for another error.
func signProblem(err error) *Problem {
	var failed *broadcastFailed
	switch {
	case errors.As(err, &failed):
		var lines []string
		for _, c := range failed.Chains {
			if c.Error != "" {
				lines = append(lines, fmt.Sprintf("%s: %s", c.Chain, c.Error))
			}
		}
		return &Problem{Code: "broadcast-failed", Message: "Broadcasting failed on " + strings.Join(lines, "; ") + ". The other chains went out."}
	case errors.Is(err, session.ErrAlreadyRefused):
		return &Problem{Code: "already-refused", Message: "This device has already refused this Sign Request, and a stance is final."}
	case errors.Is(err, session.ErrAlreadyAccepted):
		return &Problem{Code: "already-accepted", Message: "This device has already accepted this Sign Request, and a stance is final."}
	case errors.Is(err, session.ErrSignerSetFull):
		return &Problem{Code: "signer-set-full", Message: "Enough devices have already accepted; this one is not needed to sign."}
	case errors.Is(err, session.ErrUnsupported):
		return &Problem{Code: "unsupported", Message: "This device can't sign with this Key's curve."}
	case errors.Is(err, signreq.ErrUnknown):
		return &Problem{Code: "unknown-sign-request", Message: "That Sign Request is not on this device."}
	case errors.Is(err, signreq.ErrAmbiguous):
		return &Problem{Code: "ambiguous-sign-request", Message: "That names more than one Sign Request."}
	case errors.Is(err, signreq.ErrNotSigned):
		return &Problem{Code: "not-signed", Message: "That Sign Request is not signed yet, so there is nothing to broadcast."}
	case errors.Is(err, signreq.ErrCurveMismatch):
		return &Problem{Code: "curve-mismatch", Message: "That Network's chain is not signed with this Key's curve."}
	case errors.Is(err, signreq.ErrInvalid):
		// The detail is shown as the library words it, not read.
		return withDetail("invalid-request", "That Sign Request is not valid.", err)
	case errors.Is(err, chainfill.ErrBitcoin):
		return &Problem{Code: "bitcoin-fill", Message: "A Bitcoin transaction can't be filled: give its inputs and fee yourself, then propose without filling."}
	case errors.Is(err, network.ErrNoChain):
		return &Problem{Code: "no-network", Message: "This device has no Network for that chain. Add one on the Networks page."}
	case errors.Is(err, network.ErrUnknown):
		return &Problem{Code: "unknown-network", Message: "This device has no Network by that name."}
	case errors.Is(err, network.ErrInvalid):
		return withDetail("invalid-network", "This device's Networks config is not valid. Fix it on the Networks page.", err)
	case errors.Is(err, key.ErrAmbiguous):
		return &Problem{Code: "ambiguous-key", Message: "That names more than one Key."}
	}
	return nil
}

// talkProblem maps the Talk page's errors: talk, the Outbox, Keys, keygen
// Sessions and which agents are included.
func talkProblem(err error) *Problem {
	var (
		noReplica *node.NoReplicaError
		taken     *node.KeyAliasTakenError
	)
	switch {
	case errors.Is(err, errNotOpen):
		return &Problem{Code: "not-open", Message: "Another conversation was opened meanwhile, so nothing was cleared."}
	case errors.Is(err, serve.ErrBadHandle):
		return &Problem{Code: "not-included", Message: "That message mentions an agent that isn't in this conversation."}
	case errors.Is(err, cosl.ErrBadText):
		return &Problem{Code: "bad-text", Message: "That message can't be sent: it is empty, too long or not valid text."}
	case errors.Is(err, outbox.ErrCommitted):
		return &Problem{Code: "committed", Message: "That message has already been delivered, so it can't be dropped."}
	case errors.Is(err, outbox.ErrInFlight):
		return &Problem{Code: "in-flight", Message: "That message may be being delivered right now. Try again in a moment."}
	case errors.Is(err, outbox.ErrNotQueued):
		return &Problem{Code: "not-queued", Message: "That message is no longer waiting to be sent."}
	case errors.As(err, &taken):
		if taken.Key == "" {
			return &Problem{Code: "alias-taken", Message: fmt.Sprintf("“%s” is waiting on a Key being made in this Group.", taken.Alias)}
		}
		return &Problem{Code: "alias-taken", Message: fmt.Sprintf("Another Key in this Group is already called “%s”.", taken.Alias)}
	case errors.Is(err, serve.ErrBadAlias):
		return &Problem{Code: "bad-alias", Message: "That Key name can't be used: it is invalid or already taken here."}
	case errors.Is(err, session.ErrBadCurve):
		return &Problem{Code: "bad-curve", Message: "Choose ECDSA or EdDSA."}
	case errors.Is(err, session.ErrUnknownSession):
		return &Problem{Code: "unknown-session", Message: "That Key is no longer being made."}
	case errors.Is(err, session.ErrNotParticipant):
		return &Problem{Code: "not-participant", Message: "This device is not one of the devices making that Key."}
	case errors.Is(err, raftx.ErrNoLeader):
		return &Problem{Code: "no-leader", Message: "Not enough devices are online for that right now. Try again once they are."}
	case errors.Is(err, raftx.ErrNotInserted):
		return &Problem{Code: "not-inserted", Message: "That was delivered, but this device has not caught up with it yet."}
	case errors.As(err, &noReplica):
		return &Problem{Code: "no-replica", Message: "This Group's conversations aren't on this device yet. They appear once every device has joined."}
	case errors.Is(err, node.ErrUnknownProfile):
		return &Problem{Code: "unknown-profile", Message: "This device has no agent by that name."}
	case errors.Is(err, key.ErrUnknown):
		return &Problem{Code: "unknown-key", Message: "This Group has no such Key."}
	}
	return unexpected(err)
}

// unexpected is an error no case maps: its text goes in the Detail only,
// as it is a library's, not written for a person.
func unexpected(err error) *Problem {
	return withDetail("error", "Something went wrong, and DeFiOS could not do that.", err)
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
		return &Problem{Code: "bad-passphrase", Message: "That passphrase does not open this Backup."}
	case errors.Is(err, backup.ErrUnknownFormat):
		return &Problem{Code: "unknown-backup", Message: "This file is not a Backup this version of DeFiOS can read."}
	case errors.As(err, &notEmpty):
		return &Problem{Code: "config-not-empty", Message: fmt.Sprintf("A Backup restores only into an empty folder, and %s is not empty.", notEmpty.Dir)}
	case errors.As(err, &notFile):
		return &Problem{Code: "not-a-file", Message: fmt.Sprintf("%s is not a file.", notFile.Path)}
	case errors.Is(err, fs.ErrNotExist):
		return &Problem{Code: "no-file", Message: "That file is not there any more."}
	}
	return problemOf(err)
}

// backupWriteProblem is problemOf for writing a Backup file.
func backupWriteProblem(err error) *Problem {
	if errors.Is(err, fs.ErrExist) {
		return &Problem{Code: "file-exists", Message: "A file is already there. Choose a new name; a Backup never replaces a file."}
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
	msg := "Something went wrong on this computer."
	switch code {
	case "keychain":
		msg = "This computer's keychain could not be used."
	case "autostart":
		msg = "Start at login could not be changed on this computer."
	case "prefs":
		msg = "DeFiOS could not save its settings on this computer."
	}
	return withDetail(code, msg, err)
}

// configProblem is a Group's config.toml that does not load, so what a
// page lists from it can't be listed or changed: Edit TOML fixes it.
func configProblem(group string, err error) *Problem {
	return withDetail("config-error", fmt.Sprintf("This device's config.toml for %s does not load, so nothing here can be listed or changed until it is fixed with Edit TOML.", group), err)
}
