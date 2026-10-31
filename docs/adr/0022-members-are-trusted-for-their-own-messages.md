# Members are trusted for their own messages, never to decide for another Node

A threshold Key is only worth its threshold if the t Nodes that accept a Sign
Request each decide for themselves. Until now some decisions were made once,
on the COSL, for every Node: [ADR 0019](0019-providers-replace-models-on-the-cosl.md)
held that "every member of a Group is trusted", so any member could point a
Provider's `base_url` at a server of its choosing, and every other Node's
agents would send it their API key and their talk, and take back whatever
it answered as their model's reply. One member would then write every
Node's verifier. The trust model is now: **a member is trusted for its own
messages, and never to decide for another Node.** What a member says on the
COSL is that member's claim. Anything that decides how a Node acts (which
endpoint holds its credential, which model it runs, which chains it signs
for, what its agents take as given) is that Node's own config.

Providers are the first consequence: they leave the COSL and become wholly
Node-local config. [ADR 0023](0023-networks-are-node-local.md) does the same
for Networks, and [ADR 0024](0024-a-recap-is-a-local-file.md) for the Recap.
This is pre-release, so nothing is migrated.

## Considered options

- **Keep the Group trusted** (ADR 0019). Rejected: if every member could be
  trusted to decide for the others, a threshold would be pointless. A
  member who is honest about its own messages can still be wrong, coerced or
  compromised, and the threshold exists for exactly that.
- **Keep Providers on the COSL, but pin each row locally** (a Provider change
  takes effect on a Node only once its operator accepts it). Rejected: it
  keeps a shared registry whose every change needs a local approval anyway,
  so the log adds a step and no agreement. Two Nodes never need to agree on
  what a Provider is: since [ADR 0021](0021-an-agent-is-one-nodes-handle.md),
  only one Node runs an agent.
- **Restore ADR 0013's rule as it was** (a key goes only to a `base_url` this
  Node named, else the log's). Rejected for the same reason: once a local
  `base_url` is required for the key, the row on the log describes nothing a
  Node uses.

## Consequences

- **Providers are Node-local.** `[providers.<alias>]` in a Workspace's
  `config.toml` is the whole Provider: `base_url`, `api_path`, `api_key`
  (optional, as before) and `body` ([ADR 0020](0020-a-provider-has-a-base-body.md)).
  `base_url` and `api_path` keep ADR 0019's rules: both required, no
  userinfo, query or fragment, and the path's suffix picks the API Shape. A
  Conversation's `config.toml` may override a Provider table by table as
  before. An Agent Profile's `provider` names a local alias.
- **A credential goes only to an endpoint this Node configured.** The
  Provider's `base_url` is this Node's own, so its key never follows a URL
  another member wrote.
- **The `provider.*` registry ids (14, 15, 16) are retired** and stay
  reserved. `provider add` and its presets become CLI sugar that write the
  local table.
- **Talk is claims.** Another Node's messages, its agents' messages and the
  `reason` of its Sign Request are what that member says, and a Node's
  agents read them as such. Since a mention runs only on the Node whose
  message carries it ([ADR 0015](0015-agent-profiles-handles-and-agent-chains.md)),
  no member can make another Node's agent run or spend its key.
- **Rules for agents that will verify Sign Requests,** recorded now so the
  later ADR starts from them:
  - an agent judges the intent decoded from the request's bytes, and reads
    the `reason` as the proposer's claim;
  - a Node's deterministic checks (Digests derived from the request, its own
    allowlists and limits) come before any agent's opinion counts;
  - any chain state a verifier reads comes from this Node's own endpoints;
  - nothing another Node writes triggers an accept.
- **Accepted limit: a peer's snapshot can carry less talk.** A Node restored
  from a pruned peer's snapshot holds only what that peer kept
  ([ADR 0012](0012-recap-on-the-log-pruning-off-it.md)). That costs talk, not a
  decision: Pruning never touches `sign.*` or `session.*` rows, and every
  row still verifies.
