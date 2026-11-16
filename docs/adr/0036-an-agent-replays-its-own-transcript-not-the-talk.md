# An agent replays its own Transcript, not the talk

A summoned agent used to read the whole talk of its Conversation, rebuilt
from the COSL on every run (ADR 0018, "Turns by Handle"; ADR 0024). Each new
line was merged into the previous user turn, and a Recap, a prune or a filler
turn could change any part of it. So the prompt never stayed byte-identical
from one run to the next, and a provider's cache hit only on the system
block. An agent now replays **its own Transcript** instead: the record of its
earlier runs in that Conversation, exactly as they were sent and generated.
Each run appends one user turn, so an ordinary back and forth hits the cache.
The talk stays on the COSL, and the agent reads it with a Tool when it wants
to.

## Decisions

- **A Transcript is one Agent's, in one Conversation** (or Group-level talk),
  not one chain's. Every run that Agent has in that Conversation goes into its
  **live** Transcript, whichever chain or Job summoned it, with each run
  marked with its chain. Other agents see only what it posts with
  `talk_write`, never its thinking or tool calls.
- **The system block holds only what stays the same for that Agent in that
  Conversation.** It is the **DeFiOS Briefing** (embedded in the binary,
  identical for every Agent Profile), then the profile's **Agent Brief**
  (`BRIEF.md`, which replaces `SYSTEM.md`), then where the agent is answering,
  its Tools, Skills and the rules that go with them. The Briefing explains
  DeFiOS, Nodes and agents, Conversations and Group-level talk, the Vault,
  Plugins, Triggers and Networks, how depth works, and that a mention can
  summon only this Node's agents, never another Node's.
- **A run's user turn is appended after the replay**, in this order:
  1. what changed since the agent's last run (grants, Skills), if anything
  2. the talk since its last run, if any, leaving out its own lines
  3. the message that summoned it, or a Job's prompt and event
  4. its depth and the Handles it may summon

  Lines are `[seq] <display name>[/@handle]: <text>`, with `[queued]` for a
  message that has no seq yet. A new Transcript's first run carries the
  summoning message alone, with no talk before it.
- **Earlier runs are replayed in full**: thinking, tool calls and tool
  results, as recorded. Replaying only the final text would make the prefix
  differ from what was cached.
- **`say` becomes `talk_write`, and `talk_read` is added.** Both are granted
  by default. `talk_read` takes `last` (default 50) and `before_seq`, returns
  lines in the same form, marks a pruned stretch as one gap line, and says
  that lines from its own Node under its Handle are its own `talk_write`
  messages. `sign_list` takes an optional `status`.
- **Clear closes the live Transcript and starts a new one at the next run.**
  Nothing is deleted: a cleared Transcript is kept, which leaves room to
  switch between Transcripts later. A run still going finishes in the old
  one. Talk's Clear button and `conversation clear --agent <handle> | --all`
  clear Transcripts. Pruning keeps its CLI verb and policy but loses its
  button on the Talk page.
- **A change to grants, Skills, the Brief or the Briefing keeps the
  Transcript.** It costs one cache miss, and the next run's turn says what
  changed. A change of the Provider's API shape starts a new Transcript
  automatically, because recorded blocks, such as thinking signatures and
  Responses items, cannot be replayed in another shape.
- **A Transcript only grows.** The Talk page shows each Agent's live
  Transcript size. A run that fails because its context is too long says to
  clear that agent. There is no automatic compaction.

## Considered options

- **A fresh context per chain.** Rejected: it leaves the agent no memory of
  its earlier runs, and the cache hits only on the system block, which is
  where things stood before this change.
- **Only the summoning message in each turn, with `talk_read` for the rest.**
  Rejected: every run would spend a model call reading the talk, and each
  `talk_read` result would stay in the Transcript, so the same talk would be
  replayed again and again. Carrying the talk since the last run in the turn
  costs nothing extra and keeps the prefix stable.
- **Keeping per-chain Transcript files and building an Agent's history by
  filtering them by Handle.** Rejected: Clear has no single thing to act on,
  and every read has to scan every chain.
- **Replaying only the text an agent posted.** Rejected: the prefix would
  differ from what was cached.
- **Deleting a Transcript on Clear.** Rejected: Transcripts are first-class
  data, and keeping them is what makes switching between them possible later.

## Consequences

- Supersedes ADR 0018's "Turns by Handle" and its per-chain Transcripts, and
  the part of ADR 0024 that covers what agents read. The Recap no longer
  stands in for talk an agent would otherwise be given. Whether the Recap is
  kept at all is left for later.
- An agent no longer sees talk that came before its live Transcript unless it
  calls `talk_read`.
- Per-chain `transcripts/` directories are no longer read and nothing is
  migrated; `MIGRATION.md` says how to remove them and rename `SYSTEM.md`
  to `BRIEF.md`.
