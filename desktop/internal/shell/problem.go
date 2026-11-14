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
	if p := signProblem(err); p != nil {
		return p
	}
	if p := agentsProblem(err); p != nil {
		return p
	}
	if p := triggersProblem(err); p != nil {
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
		return &Problem{"broadcast-failed", "Broadcasting failed on " + strings.Join(lines, "; ") + ". The other chains went out."}
	case errors.Is(err, session.ErrAlreadyRefused):
		return &Problem{"already-refused", "This device has already refused this Sign Request, and a stance is final."}
	case errors.Is(err, session.ErrAlreadyAccepted):
		return &Problem{"already-accepted", "This device has already accepted this Sign Request, and a stance is final."}
	case errors.Is(err, session.ErrSignerSetFull):
		return &Problem{"signer-set-full", "Enough devices have already accepted; this one is not needed to sign."}
	case errors.Is(err, session.ErrUnsupported):
		return &Problem{"unsupported", "This device can't sign with this Key's curve."}
	case errors.Is(err, signreq.ErrUnknown):
		return &Problem{"unknown-sign-request", "That Sign Request is not on this device."}
	case errors.Is(err, signreq.ErrAmbiguous):
		return &Problem{"ambiguous-sign-request", "That names more than one Sign Request."}
	case errors.Is(err, signreq.ErrNotSigned):
		return &Problem{"not-signed", "That Sign Request is not signed yet, so there is nothing to broadcast."}
	case errors.Is(err, signreq.ErrCurveMismatch):
		return &Problem{"curve-mismatch", "That Network's chain is not signed with this Key's curve."}
	case errors.Is(err, signreq.ErrInvalid):
		// The detail is shown as the library words it, not read.
		return &Problem{"invalid-request", "That Sign Request is not valid (" + err.Error() + ")."}
	case errors.Is(err, chainfill.ErrBitcoin):
		return &Problem{"bitcoin-fill", "A Bitcoin transaction can't be filled: give its inputs and fee yourself, then propose without filling."}
	case errors.Is(err, network.ErrNoChain):
		return &Problem{"no-network", "This device has no Network for that chain. Add one on the Networks page."}
	case errors.Is(err, network.ErrUnknown):
		return &Problem{"unknown-network", "This device has no Network by that name."}
	case errors.Is(err, network.ErrInvalid):
		return &Problem{"invalid-network", "This device's Networks config is not valid (" + err.Error() + "). Fix it on the Networks page."}
	case errors.Is(err, key.ErrAmbiguous):
		return &Problem{"ambiguous-key", "That names more than one Key."}
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
		return &Problem{"not-open", "Another conversation was opened meanwhile, so nothing was cleared."}
	case errors.Is(err, serve.ErrBadHandle):
		return &Problem{"not-included", "That message mentions an agent that isn't in this conversation."}
	case errors.Is(err, cosl.ErrBadText):
		return &Problem{"bad-text", "That message can't be sent: it is empty, too long or not valid text."}
	case errors.Is(err, outbox.ErrCommitted):
		return &Problem{"committed", "That message has already been delivered, so it can't be dropped."}
	case errors.Is(err, outbox.ErrInFlight):
		return &Problem{"in-flight", "That message may be being delivered right now. Try again in a moment."}
	case errors.Is(err, outbox.ErrNotQueued):
		return &Problem{"not-queued", "That message is no longer waiting to be sent."}
	case errors.As(err, &taken):
		if taken.Key == "" {
			return &Problem{"alias-taken", fmt.Sprintf("“%s” is waiting on a Key being made in this Group.", taken.Alias)}
		}
		return &Problem{"alias-taken", fmt.Sprintf("Another Key in this Group is already called “%s”.", taken.Alias)}
	case errors.Is(err, serve.ErrBadAlias):
		return &Problem{"bad-alias", "That Key name can't be used: it is invalid or already taken here."}
	case errors.Is(err, session.ErrBadCurve):
		return &Problem{"bad-curve", "Choose ECDSA or EdDSA."}
	case errors.Is(err, session.ErrUnknownSession):
		return &Problem{"unknown-session", "That Key is no longer being made."}
	case errors.Is(err, session.ErrNotParticipant):
		return &Problem{"not-participant", "This device is not one of the devices making that Key."}
	case errors.Is(err, raftx.ErrNoLeader):
		return &Problem{"no-leader", "Not enough devices are online for that right now. Try again once they are."}
	case errors.Is(err, raftx.ErrNotInserted):
		return &Problem{"not-inserted", "That was delivered, but this device has not caught up with it yet."}
	case errors.As(err, &noReplica):
		return &Problem{"no-replica", "This Group's conversations aren't on this device yet. They appear once every device has joined."}
	case errors.Is(err, node.ErrUnknownProfile):
		return &Problem{"unknown-profile", "This device has no agent by that name."}
	case errors.Is(err, key.ErrUnknown):
		return &Problem{"unknown-key", "This Group has no such Key."}
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

// backupWriteProblem is problemOf for writing a Backup file.
func backupWriteProblem(err error) *Problem {
	if errors.Is(err, fs.ErrExist) {
		return &Problem{"file-exists", "A file is already there. Choose a new name; a Backup never replaces a file."}
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
