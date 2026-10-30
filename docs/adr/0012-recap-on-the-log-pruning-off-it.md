# Conversations compact by Recap on the log and Pruning off it

A Conversation's history is bounded by two deliberately separate mechanisms: a
**Recap** (`app.recap`) is a COSL message that accounts for talk up to a stated
seq and stands in place of it for readers, and **Pruning** is one Node's local
destruction of the `app.freeform` payloads a Recap has already accounted for.
Recap is consensus and reversible; Pruning is Node-local and is not. Both are
operator verbs; neither fires on a timer. Tickets to follow.

Measured first: 500k tokens of talk is 1.91 MB of text but 2.3–26 MB of
replica, depending entirely on message count — the per-row floor is ~219 bytes
of envelope and ~514 bytes of sqlite row, so text is a minority of the bytes
until messages exceed roughly 60 tokens. Snapshots are whole-replica
`VACUUM INTO` copies retained twice over, so steady state is about 3× history
per Node, and a Node that falls past `SnapshotThreshold` re-transfers all of it.

## Considered options

- **Checkpointed genesis** (truncate the chain at a signed base and verify
  from there) — rejected: it needs a trust model for the base hash, it loses
  the derived state of every `session.*` and `sign.*` row below the base, and
  because the Roster is fixed at formation
  ([ADR 0002](0002-group-formation.md)) a Node that falls below the base has
  no re-genesis path and is simply dead. Pruning keeps every row and so keeps
  `isPrefixOf` working between a pruned and an unpruned replica.
- **A local hidden-below-seq view only** — the position
  [ADR 0004](0004-conversation-on-cosl.md) took ("clearing a Conversation for
  context is a local view, not a COSL delete") — rejected and superseded: a
  per-Node view gives the Group no shared account of what was said and gives a
  future agent nothing to load instead of the history. The Recap is that
  shared account, and it is a message, not a view.
- **Mutable rows / a COSL delete** — rejected for the reason
  [ADR 0008](0008-networks-on-cosl.md) gives: the replica is append-only
  (triggers block UPDATE and DELETE,
  [ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md)). A Recap supersedes
  by fold, exactly as `network.update` does by alias.
- **Carrying every covered row's hash in the Recap payload** — rejected: 50k
  covered rows is 1.6 MB of hashes inside a 64 KB message, and it buys little
  while every covered row is still readable through `cosl log`. Raft order is
  the authority, as it is for Networks. Revisit if Pruning ever needs to be
  audited by a Node that cannot see the originals.
- **One coupled `recap and prune after N` policy** — rejected: a Node-local
  threshold would fire a Group-wide append, which makes the useful archive
  role (generates Recaps, keeps everything) unreachable and lets whichever
  Node fires first reset every other Node's counter.
- **Thresholds denominated in tokens** — rejected: there is no tokenizer in
  the tree and none in `go.mod`, so `serve` cannot evaluate one. Thresholds
  are deferred entirely rather than measured in a bytes/4 fudge wearing the
  word "tokens".
- **`serve` summarizing autonomously** — rejected: a custody daemon does not
  embed a model client. The verb takes the text; whatever produced it stays
  outside the binary.
- **FSM rejection of a non-monotonic Recap** — rejected: the deterministic
  fold skip is the established pattern ([ADR 0008](0008-networks-on-cosl.md)),
  it needs no new FSM precondition machinery, and `verify` stays purely about
  envelope integrity.
- **A `chain_version` field and a migration** — rejected: there is no deployed
  state to migrate. The chain hash changes shape once, everything stays
  labelled v1, and no Node ever reads a v1 chain.

## Consequences

- One new registry type, `app.recap`, id 13: payload is `text` (UTF-8, ≤64 KB,
  as `app.freeform`) and `base_seq`, the highest seq it accounts for. The
  Conversation stays on the envelope's `conversation_id`, unset for
  Group-level talk, so `idx_cosl_conversation` already indexes it.
- `chainHash` folds over `sha256(envelope)` rather than the envelope bytes,
  and the replica gains an `envelope_hash` column. This is what lets a pruned
  row still verify from genesis, and it is the one part of this work that
  touches the chain's integrity code — it lands before either verb.
- A Recap's `base_seq` must exceed the live Recap's for that Conversation, and
  must name an earlier `app.freeform` of that same Conversation, so a Recap
  never covers another Conversation's rows. The fold enforces both by
  skipping, as it does a Recap whose payload is undecodable or whose text
  `app.freeform` would refuse (the FSM does not parse payloads). The writer
  pre-checks so an operator gets an error, and accepts the same stale-read
  race the Networks writer has.
- Pruning destroys `app.freeform` payloads only. Recaps keep their text
  forever — each is at most 64 KB and they arrive once per window, so the
  ladder of historical accounts is nearly free.
- Pruning is the one UPDATE the append-only triggers admit: an
  `app.freeform`'s envelope set to NULL with every other column unchanged.
  A replica's triggers are always its own binary's, reinstalled on open and
  after a snapshot restore. Pruning tells no Node, but a snapshot a pruned
  Node ships carries no payload it destroyed, so a Node restored from it
  holds that much less talk.
- Pruning never reaches talk no Recap has accounted for: the prunable set is
  the rows at or below the live Recap's `base_seq`. `conversation prune` with
  no Recap errors and points at `conversation recap`.
- `prune = true` on a Node's per-Group `config.toml` (a TOML boolean, so
  `prune = on` does not parse) means that Node holds no superseded
  `app.freeform` payload: `serve` re-applies it on startup, when a Recap
  commits and after an `InstallSnapshot` restore, because a restore from an
  archival Node resurrects everything that Node still has, and logs what each
  prune dropped. The verb overrides the policy and warns when it is off,
  since a manual prune on such a Node is undone by the next restore. Default
  off: a custody tool should not destroy payloads the first time it syncs.
- `conversation log` folds by default; `--full` shows the superseded rows with
  pruned ones as metadata-only placeholders; `--lean` emits only
  `<display name>: <text>` lines, the live Recap then everything after it, for
  an agent's initial context. `cosl log` never folds.
- Amends [ADR 0009](0009-sign-request-on-cosl.md): `SignRequest.reason_ref`
  and `MessageRef` are removed and field 4 reserved. The required `reason`
  already carries the justification and lives in a payload that is never
  pruned, while a ref often pointed at an unremarkable line ("yes, make the
  sign request"). Removing it deletes the only reason Pruning would have
  needed an exemption set, so the rule is one message type and one condition.
- Three new registry types for the model a future agent uses,
  `model.create` / `model.update` / `model.remove`, ids 14/15/16: alias-keyed
  and folded exactly as Networks are, so the Group agrees on what produced a
  Recap. A Conversation's agent config names the alias rather than pinning a
  config id — unlike a Sign Request, a Recap commits to nothing. A config is
  a `provider` (`anthropic` | `openai`, fixed for the alias's life as a
  Network's type is), the provider's `model` id, and an optional `base_url`
  and `api_path`; neither may carry userinfo or a query, so a credential
  cannot ride along.
  *Superseded by [ADR 0019](0019-providers-replace-models-on-the-cosl.md): ids 14/15/16 are
  `provider.create` / `provider.update` / `provider.remove`, a config of
  `base_url` and `api_path` only; the model id lives in the Agent Profile.*
- Credentials for those models live in the Vault, with an environment variable
  taking precedence when set and `init` never writing one — the `TS_AUTHKEY`
  precedent in `config.toml`. This is a deliberate departure from
  [ADR 0008](0008-networks-on-cosl.md), which put key-bearing RPC URLs in the
  plaintext config directory; encrypting keyshares while leaving a credential
  that can drain a billing account in plaintext is inconsistent, and one
  secret store is the point. Moving the RPC credentials there is a follow-up.
  *Amended by [ADR 0013](0013-group-config-workspaces-and-named-secrets.md):
  the Vault holds Secrets by name, not by Model alias, and `config.toml`
  refers to them by provider (`[providers.<p>] api_key = "$NAME"`), so a
  removed and re-added alias cannot inherit another provider's key. The RPC
  credentials move in the same change.*
- Per-Conversation agent config is groundwork only: the path
  `config/group/<alias>/conversation/<conversation-id>/` exists, with
  `group-level` reserved for nil-id talk (a Conversation ID is always 64 hex
  characters, so it cannot collide). Nothing parses a file inside it yet; the
  intended contents are a model alias, a token window, and a `soul.md`
  describing the agent's purpose. Model settings are Group-scoped, agent
  settings Conversation-scoped. *Amended by
  [ADR 0013](0013-group-config-workspaces-and-named-secrets.md): a
  Conversation's Workspace is `config/group/<alias>/<curve>-<pubkey-prefix>/`, its
  `config.toml` overrides the Group's `[networks]`, `[providers]` and `[agent]`
  table by table, the markdown is `SOUL.md`, and `group-level` is gone: the
  Group Workspace itself configures Group-level talk.*
- `GLOSSARY.md` gains **Recap** and **Pruning**. "Tombstone" keeps its
  Networks meaning — a removal marker that destroys nothing — and is not used
  for Pruning.
