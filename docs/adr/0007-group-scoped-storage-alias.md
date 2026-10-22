# Group-scoped storage keyed by a local alias; the Vault is one portable directory

Each Group's config and data live under a per-Group subtree named by a **local,
user-chosen alias** rather than the Group ID, because the Group ID is derived
from the roster and does not exist until formation succeeds, while the subtree
(tailnet config, tsnet state) must exist *first* so the Node can join the tailnet
and discover peers. The Vault becomes a single portable directory holding the
Node's shared secrets plus each Group's shares in **separate** age files;
everything else a Node stores stays plaintext and inspectable without `serve`.
Extends [ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md); enables
[ADR 0006](0006-multiple-groups-per-node.md).

## Considered options

- **Name subtrees by Group ID** (as the MVP's `group/<id>/`) — rejected: the ID
  is unknown until formation, yet `config.toml` and tsnet state are needed to
  bring up the tailnet and run `node list` / `group create` beforehand; and a
  64-char hex string is a poor thing to type for `group use` / `--group`. The
  Group ID stays canonical on the wire (`roster.json`, COSL rows, Group ID
  derivation); the alias is purely local and never leaves the machine.
- **Rename the subtree to `<group_id>` once formation completes** — rejected:
  it churns paths and open file handles for no gain the alias-to-ID mapping
  already in `roster.json` doesn't provide.
- **One monolithic Vault file holding every Group's shares** — rejected: every
  Group's `serve` would then decrypt every other Group's shares into memory,
  dissolving the process isolation [ADR 0006](0006-multiple-groups-per-node.md)
  buys. Separate age files (same scrypt work factor and passphrase) let a
  `serve` open only the shared-secrets file plus its own Group's file.
- **Scatter each Group's shares into its own `group/<alias>/` subtree** —
  rejected: the Vault should be one portable backup/move unit. Backup of a Node
  is `identity` plus the single `vault/` directory, nothing more.
- **Encrypt the roster, COSL, and config at rest** — rejected: point-to-point
  Session payloads are already sealed
  ([ADR 0001](0001-cosl-message-envelope.md),
  [ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md)), and leaving these
  plaintext is exactly what lets `group list` / `group inspect`, `cosl log`, and
  `key list` work for a Group whose `serve` is not running.

## Consequences

- Node root holds `identity` (plaintext) and `vault/` (shared secrets at its
  root, one age file per Group for that Group's shares). Each
  `group/<alias>/` subtree holds `config.toml` (headscale URL, hostname,
  display name), `roster.json`, `cosl.sqlite`, `raft.db`, `raft-snap/`, and
  `tsnet/`.
- `init` writes `identity` + `vault/` only; a new **`group new <alias>`** verb
  scaffolds a Group and its `config.toml` before any network and auto-focuses
  it. `display_name` becomes per-Group (defaulting from `init --name`).
- Group aliases are local, lowercase, filesystem-safe, and unique on the Node;
  they never appear on the wire.
- `GLOSSARY.md` gains **Group alias**; the **Vault** entry is updated to the
  directory form.
