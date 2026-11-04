# A Backup is the Node minus what peers give back

A lost machine is a lost Node: the only way back today is `init --auth-key`,
which restores the auth seed but mints a new encryption keypair and leaves
every Key share and Secret behind, so it never brought the Node back at all.
[ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md) said a Node's backup is
`identity` + `vault`, but since [ADR 0007](0007-group-scoped-storage-alias.md)
the Vault's per-Group files are named by alias, and only each Group's Record
(`roster.json`) binds an alias to its Group ID; the Vault alone is orphaned
shares.

A **Backup** is now one file: an age (scrypt) archive, sealed under the Vault
passphrase in force when it is taken, holding this Node's `identity`, the
Vault's age files byte for byte, each Group's Record, and the config tree
(Group Workspaces and `config.toml`, Agent Profiles, `display-name`,
`current-group`) less every `RECAP.md` and Transcript. `defios backup <path>`
writes one; `defios init --from-backup <path>` restores one; `init --auth-key`
is removed. `defios passphrase change` re-seals the Vault under a new Vault
passphrase. This is pre-release, so nothing is migrated.

## Considered options

- **The Vault alone.** Rejected: without the Records, a restored
  `shares-<alias>` names a Group the Node no longer knows it is in.
- **Everything on disk**, the COSL replica, Raft state, tsnet state, Outbox
  and Transcripts included. Rejected: Raft `InstallSnapshot` already rebuilds
  the replica from peers (ADR 0005); copied tsnet state is a second machine
  claiming the first's tailnet identity; a restored Outbox would commit its
  queued messages a second time if the original also drained it
  ([ADR 0025](0025-an-outbox-holds-what-a-node-writes-without-a-quorum.md));
  Recaps and Transcripts are one machine's working memory, large, and never
  needed to be the Node.
- **Plaintext Vault contents inside the outer seal.** Rejected: the Vault's
  files go in as sealed, so a restore writes back exactly the bytes that
  unlocked before. `backup` opens every one with the passphrase given first,
  and refuses if any fails, so no Backup carries a Vault that will not open.
- **Only the Vault encrypted, the rest plaintext.** Rejected: the config
  sits beside Secrets, and a plaintext Roster still says who this Node holds
  custody with.
- **A separate Backup passphrase.** Rejected: one secret to remember; the
  restored Node keeps the passphrase its Backup was sealed under.
- **Detecting two live copies of one Node.** Rejected: a restored Node is
  the same Node ID, auth keypair and encryption keypair, so nothing on the
  wire tells the copies apart. The original must never run again; `init
  --from-backup` says so, and the rest is the operator's.
- **Tracking the last Backup and warning on keygen.** Rejected: more state
  and nagging for something the operator controls. A Backup taken before a
  keygen has no share of that Key; `backup` lists the Keys it holds, and the
  docs encourage a Backup after every new Key.
- **Handing a running `serve` the new passphrase over the socket.**
  Rejected: a passphrase-bearing control message for little gain.
  `passphrase change` refuses while `serve.sock` answers, because `serve`
  holds the old passphrase and reseals a Group's shares with it on keygen.
- **Re-sealing the Vault file by file in place.** Rejected: a crash midway
  leaves a Vault under two passphrases.

## Consequences

- **Staged swap.** `passphrase change` opens every Vault file under the old
  passphrase, seals them all under the new one into `vault.new/`, reopens
  every file there under the new one, and only then renames `vault/` to
  `vault.old/`, `vault.new/` to `vault/`, and deletes `vault.old/`. Any
  command touching the Vault refuses while a `vault.new/` or `vault.old/` is
  left over, naming it; `passphrase change` offers to finish the swap once
  `vault.new/` opens under the new passphrase.
- **Passphrase sources.** The old passphrase comes as before
  (`--passphrase-file`, `DEFIOS_PASSPHRASE`, or a prompt); the new one from
  `--new-passphrase-file` or a prompt asked twice, never from the
  environment. A new passphrase equal to the old is refused. Existing
  Backups are not re-sealed and still open with the old passphrase; the
  command says so.
- **Atomic writes.** `vault.Seal` and Record writes go through a temp file
  and a rename, so `backup` may run while `serve` does and never reads half a
  file — a crash-safety gap that existed regardless.
- **Restore is not a merge.** `init --from-backup` refuses when a Node exists
  or when the config directory is not empty, refuses `--name`, and writes
  into this machine's resolved paths, not the original's. It prints the two
  steps it cannot take: a fresh tailnet preauth key for
  `tailnet_auth_key`, and removing the original's registration in
  headscale.
- **Format.** The archive carries a format version; a restore refuses one
  it does not know. `backup` refuses to overwrite an existing file and writes
  it `0600`, and lists its Node ID, Groups and Keys (`--json` likewise).
- Amends ADR 0005: a Node's backup is a Backup, not `identity` + `vault`.
- `GLOSSARY.md` gains **Backup** and **Vault passphrase**.
