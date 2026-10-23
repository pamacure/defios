# Blockchain Networks are per-Group configs on the COSL, recalled by alias

A Network is a blockchain a Group may broadcast a signed transaction to
(Bitcoin, Ethereum, or Solana). Its config lives on the Group's COSL as
`network.create` / `network.update` / `network.remove` messages, which the FSM
folds into a current map keyed by a local **alias** — the same append-only,
fold-to-current-state pattern the log already uses for Keys and Group records.
Every Node in the Group therefore agrees, by Raft order, on exactly what
"`ethereum-mainnet`" means before anyone signs for it. Design captured in the
[Signing Design doc](https://claude.ai/artifact/1hHd6qfr2nUpq1Evp3jnBz);
tickets to follow.

## Considered options

- **Local-only config (per-Node JSON / CLI)** — rejected: a Network determines
  *where a signed transaction can be broadcast* and binds into a Sign Request.
  Two Nodes disagreeing on what Network `X` is (chain id, Bitcoin network,
  Solana cluster) while signing the same request is a correctness hazard, not a
  convenience bug. Consensus on the log removes it.
- **A cross-Group global catalog** — rejected: the COSL is itself Group-scoped
  (every envelope carries `group_id`), so "replicated and identical across the
  Group" already means per-Group. A Group curates its own Network list; there is
  no shared registry to keep in sync across Groups.
- **Mutable rows (update/delete in place)** — rejected: the COSL replica is
  append-only (triggers block UPDATE/DELETE,
  [ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md)). CRUD is expressed as
  new messages; "edit" supersedes by alias, "remove" tombstones, current state
  is the fold.

## Consequences

- Three new COSL registry types (`network.create` / `network.update` /
  `network.remove`); ids 8, 9, 10 (id 7 stays
  reserved for `sign.request`, [ADR 0009](0009-sign-request-on-cosl.md)). Each is one line in
  `internal/cosl/registry.go` plus a Summary case.
- A Network's `type` fixes the curve a signing Key must use for it
  (bitcoin/ethereum → secp256k1, solana → Ed25519). Sign Request creation
  validates the chosen Key's curve against every referenced Network
  ([ADR 0009](0009-sign-request-on-cosl.md)).
- **RPC endpoints live on the Network config** (so broadcast works for any
  Node), with an array permitted for failover. Because provider URLs often embed
  API keys that would then replicate in plaintext to every Node, a Node may set
  a **local override** that wins when it broadcasts. The override lives in the
  Node's config directory (`$XDG_CONFIG_HOME`), not the Vault and not the log:
  `group/<group-alias>/networks.toml`, one `[<network-alias>]` table with
  `rpc_urls = [...]`.
- The fold is deterministic and skips messages that do not apply to the state
  they meet: a create of a live alias (first committed wins), an update of an
  absent alias, an update that changes the Network's type, a remove of an absent
  alias, and invalid configs. The writer checks the same preconditions so an
  operator gets an error instead of a silent skip. A removed alias may be
  created again.
- Alias follows the Group-alias convention (`^[a-z0-9][a-z0-9_-]{0,63}$`,
  [ADR 0007](0007-group-scoped-storage-alias.md)) and is local naming only; the
  Network's on-wire identity is its config fields, not the alias.
