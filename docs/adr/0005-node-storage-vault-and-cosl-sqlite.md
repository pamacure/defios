# Age Vault for secrets; plaintext COSL sqlite; engine Start waits on local accept

A Node's secrets live in a passphrase-encrypted Vault; the COSL replica stays
plaintext sqlite because point-to-point payloads are already sealed. `serve`
holds the unlocked Vault in memory and talks to local CLI over a unix socket;
Node-to-Node traffic stays tsnet. Incoming Sessions do not `Start()` the engine
until the local operator accepts — an unattended `serve` must not rubberstamp a
peer's `session.start`. Decided in
[defios#8](https://github.com/pamacure/defios/issues/8).

## Considered options

- **Encrypted sqlite / SQLCipher** — rejected: key material is not in the
  replica, SQLCipher is cgo, and
  [ADR 0001](0001-cosl-message-envelope.md) already seals p2p `tss.session`
  rows so a stolen disk cannot reassemble both halves of a 2-of-2 Key from
  sqlite alone.
- **OS keyring / passkey / biometric** — map Notes put the user-end auth-key
  mechanism out of scope. MVP is `filippo.io/age` scrypt around a versioned
  protobuf. DIY NaCl secretbox + Argon2 rejected: age is a specified format
  with a maintained Go library.
- **Recompute Node ID from the current auth public key on unlock** — rejected:
  Node ID is written once at init and must survive auth-key rotation so the
  Node stays the same across Groups
  ([ADR 0002](0002-group-formation.md)).
- **Persist a share only after n matching `session.complete`s** — rejected: a
  crash after this Node has attested on the log but before the peer completes
  would mint a Key this Node no longer holds. Write the share to the Vault
  when `endCh` fires, before appending `session.complete`; discard on abort
  or mismatch.
- **`mattn/go-sqlite3`** — rejected: cgo. `modernc.org/sqlite` keeps the CLI
  pure-Go (same constraint as later wazero / Android).
- **A manual resync verb for long-offline Nodes** — rejected: Raft
  `InstallSnapshot` already restores an empty `group/<id>/` as long as the
  Vault survived and a majority still has the replica. Lost Vault is a new
  Node.
- **CLI control on tsnet** — rejected: that is the rubberstamp path. Unix
  socket is local IPC only (`0600`, never on the tailnet). Compromising this
  uid is losing this Node, which threshold is meant to survive.
- **A COSL `session.accept` type** — rejected: membership is still the
  membership gate
  ([ADR 0003](0003-keygen-session-orchestration.md)). Presence is local
  policy — replica apply is automatic, Vault use waits on accept.

## Consequences

- Layout is XDG: config in `$XDG_CONFIG_HOME/defios`, data (identity, Vault,
  tsnet, `group/<id>/`) in `$XDG_DATA_HOME/defios`, `serve.sock` in
  `$XDG_RUNTIME_DIR`. Lookup: `--dir`, else `DEFIOS_CONFIG_DIR` /
  `DEFIOS_DATA_DIR`, else XDG defaults. Backup of this Node is `identity` +
  `vault`; `group/` is disposable.
- COSL schema, append-only triggers, and catch-up bookkeeping live in the
  ticket resolution; `conversation log` is a `conversation_id` filter, not a
  second table
  ([ADR 0004](0004-conversation-on-cosl.md)).
  *Amended by [ADR 0012](0012-recap-on-the-log-pruning-off-it.md): the
  triggers admit one UPDATE, Pruning (an `app.freeform`'s envelope set to
  NULL, every other column unchanged), and `Open` and a snapshot restore
  reinstall them, so a replica's guard is never a snapshot's.*
- Amends [ADR 0003](0003-keygen-session-orchestration.md): engine `Start()`
  on apply **and** local accept. The initiator accepted by submitting.
- Reads of plaintext identity and sqlite work locked and without `serve`.
- `GLOSSARY.md` gains **Vault**.

