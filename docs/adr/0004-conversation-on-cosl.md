# Conversations are COSL app.freeform, split by derived Conversation ID

Human talk rides the COSL, not a parallel tsnet protocol: Group-level freeform
and Key-scoped Conversation are the same `app.freeform` type, distinguished
only by `conversation_id`. Histories match because they are the replica;
catch-up is Raft. Decided in
[defios#7](https://github.com/pamacure/defios/issues/7).

## Considered options

- **Direct tsnet fan-out, never on the COSL** (the map's original transport
  note) — rejected: replicas would diverge, an offline Node could not catch
  up, and a 2-Node Group cannot leave mail on a log that requires both Nodes
  to commit.
- **Hybrid: live fan-out, COSL fallback when a hop fails** — rejected: two
  orders (arrival vs `seq`), duplicate frames, and the fallback still cannot
  commit during a 2-Node outage.
- **COSL commit waits on the Key's t** — rejected: t is how many shares make
  a Key signature (post-MVP); Raft majority of `|Roster|` is when a row is
  canon. A 4-of-5 Key would mute chat and `session.abort` whenever four Nodes
  were not up. Two quorums stay two quorums.
- **Minted `conversation_id`**
  ([ADR 0001](0001-cosl-message-envelope.md)) — rejected: one Conversation per
  Key, so the id is `SHA-256("defios-conversation-id-v1" || curve || joint
  public key)`, matching Node ID / Group ID derivation. No create round.

## Consequences

- `conversation send` / `conversation send --key` / `conversation log` append
  and read `app.freeform`. Nil conversation id is Group-level talk; set is
  that Key's Conversation. Membership is the Roster; the Conversation exists
  iff the Key exists. Encryption keypair is unused (broadcast plaintext);
  authorship is the envelope's auth signature.
- MVP protocol types other than Key-scoped `app.freeform` keep a nil
  conversation id — keygen is a Session, not a Key. Future signing Sessions
  are the first protocol types that would set it.
- Amends [ADR 0001](0001-cosl-message-envelope.md): `conversation_id` is
  derived, not minted; Conversation rows *are* COSL messages, not links to an
  off-log channel.
- Amends [ADR 0003](0003-keygen-session-orchestration.md): `session.start`
  accepts t only when `2t > n` (disjoint signing sets forbidden). That is Key
  policy, not a COSL commit rule.
- Schema queries stay on
  [Design Node storage: encrypted keyshare container and COSL sqlite schema](https://github.com/pamacure/defios/issues/8)
  ([ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md));
  no second sqlite history. Clearing a Conversation for context is a local
  view, not a COSL delete.

