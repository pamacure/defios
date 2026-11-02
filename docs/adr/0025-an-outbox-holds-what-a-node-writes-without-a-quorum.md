# An Outbox holds what a Node writes without a quorum

A row is on the COSL only once a majority of the Roster commits it, so a Node
whose peers are down cannot write at all: in a 2-Node Group, one Node's
`serve` being off silences the other, which is exactly the gap
[ADR 0004](0004-conversation-on-cosl.md) named ("a 2-Node Group cannot leave
mail on a log that requires both Nodes to commit"). An always-on Node, a
server that proposes Sign Requests at odd hours, should be able to write
while the others sleep. Catching up is not the problem: a Node that was away
is already brought up to date by Raft's AppendEntries or `InstallSnapshot`
([ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md)).

Each Node now keeps an **Outbox** per Group: the messages it has signed but
could not commit because no leader was known. `serve` drains it into Raft, in
the order it was written, as soon as a leader is. The COSL, the FSM, Raft and
the Roster are unchanged. This Node's agents read its queued messages as if
they had been said, so a chain runs end to end with no leader. This is
pre-release, so nothing is migrated.

## Considered options

- **A witness voter**: a Raft voter with no keyshare, so 2 of 3 commit while
  one Node is down. Rejected: it changes the Roster, the Group ID derivation
  ([ADR 0002](0002-group-formation.md)) and "Roster fixed at formation", and
  in a 2-of-2 the absent Node must still come up to sign.
- **An Inbox: per-recipient delivery with a record of who received what.**
  Rejected for the reason ADR 0004 rejected live fan-out: two orders (arrival
  and seq) and duplicate frames. Raft replication is the delivery.
- **Peers hold a Node's queued messages and forward them.** Envelopes are
  signed, so any Node could forward one ([ADR 0001](0001-cosl-message-envelope.md)).
  Rejected: the Node writing is the one that is up, by assumption, and another
  Node's disk would hold its unsent intents. An Outbox is its author's alone.
- **Sign at drain, not at queue time**, so a Solana request takes a fresh
  `recent_blockhash` when it commits. Rejected: `created_at` is set at signing,
  so a Sign Request's deadline would count from when a quorum returned rather
  than from the request itself; `serve` would rewrite a transaction, and its
  Digests, after its operator approved it; and the returning Nodes would still
  have the same 60–90 seconds to accept and sign. Durable nonces would be the
  Solana answer, and are out of scope
  ([`.out-of-scope/solana-durable-nonces.md`](../../.out-of-scope/solana-durable-nonces.md)).
- **Transcripts as the agents' source of truth.** Rejected: a Transcript holds
  only this Node's agents' output, never another Node's talk, and feeding it
  back would put thinking and tool calls into later turns, a separate decision
  about what an agent remembers.

## Consequences

- **What may queue.** `app.freeform`, `sign.request`, `sign.accept` and
  `sign.broadcast`. Every other type, Session traffic above all, fails fast
  with `no-leader` as before. A `sign.request` queues whatever its chains,
  but one with a Solana transaction commits only if a majority returns
  before its deadline (60 seconds by default, about as long as its
  `recent_blockhash` lives); otherwise it is dropped as expired unsent.
- **Queueing is automatic.** When a queueable write finds no leader, `serve`
  queues it and answers `queued`, with an exit code distinct from a commit's;
  nothing reports a queued message as committed. While the Outbox is not
  empty every queueable write joins its back, leader or not, so a Node's own
  messages commit in the order it wrote them.
- **The envelope is signed when queued.** `created_at` is the time of writing,
  and a Sign Request's deadline counts from it, as for any request. A queued
  request whose deadline passes is dropped from the Outbox without committing
  and logged as expired unsent: a dead request never reaches the log.
- **The Outbox persists**, per Group, in plaintext beside the replica and
  outside its append-only table (every queueable type is broadcast plaintext
  on the log anyway). `serve` drains it on start, when a leader becomes
  known, and on its one-minute fallback tick
  ([ADR 0014](0014-serve-reads-the-cosl-on-change.md)). A message the FSM
  rejects (`ErrNotInserted`) or that has expired is logged and skipped; the
  drain goes on.
- **A rejoin overwrites nothing.** The Outbox is outside Raft. If a majority
  elsewhere kept committing while this Node was cut off, this Node's queued
  messages commit after those rows, so the final order may differ from the
  order this Node showed while it waited. Talk is claims
  ([ADR 0022](0022-members-are-trusted-for-their-own-messages.md)), so an
  agent that answered without those rows answered what it saw.
- **This Node's view of the talk** is its committed talk followed by its
  Outbox, in queue order. A summoned agent's talk is that view, after this
  Node's `RECAP.md` as before ([ADR 0024](0024-a-recap-is-a-local-file.md)),
  and `conversation log` shows it, queued messages marked `queued` with no
  seq (a JSONL line with `"queued": true`,
  [ADR 0017](0017-log-readers-print-jsonl.md)). `cosl log` and `sign list`
  stay committed-only: they are the log.
- **Agents no longer wait on Raft.** A chain starts when a person's message
  that mentions agents is written, committed or queued, and never again when
  a queued one commits. An agent's say is written, committed or queued, and
  the next agent reads it from the view at once; the wait for the replica to
  apply it goes. Amends [ADR 0015](0015-agent-profiles-handles-and-agent-chains.md)
  and [ADR 0018](0018-an-agent-says-one-message.md), where each reply commits
  before the next agent starts.
- **The Outbox also holds what this Node has sent**: a message it committed
  stays there, with its seq, until this Node's replica applies it, as on a
  follower until the leader's next heartbeat. So its agents never wait on
  Raft and still see every message this Node wrote. Only queued messages
  persist; a restart reopens the replica, which shows what it has applied.
- **A chain is named by msg_id**, the msg_id of the person's message that
  starts it, not its seq, which a queued message does not have: the
  Transcript's file name, `conversation retry --chain` and
  `conversation cancel`, and chain events. A unique hex prefix is accepted.
  The `said` outcome and `agent.commit` carry the said message's msg_id, and
  its seq when it has one.
- **`outbox list` and `outbox drop <msg_id>`** show and withdraw this Node's
  queued messages. A queued message was never on the log, so dropping one
  erases it; dropping a message drops nothing else, not even a queued reply
  to it.
- **Pruning** never touches the Outbox; it acts on committed rows only.
