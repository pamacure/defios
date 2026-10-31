# A Recap is a local file; Pruning stands alone

[ADR 0012](0012-recap-on-the-log-pruning-off-it.md) made a Recap a COSL
message, `app.recap`, that every Node read in place of the talk it covered,
and allowed Pruning only of talk a Recap had covered. Under the trust model
of [ADR 0022](0022-members-are-trusted-for-their-own-messages.md) a Node
trusts only its own Recap, so a Recap on the log is replicated to every Node
for one Node's use, and shows the others how that Node frames the talk. A
**Recap** is now a file, `RECAP.md`, in the Conversation's data directory on
the Node that wrote it: its account of the talk up to a stated seq, replaced
whole each time it is written. **Pruning** no longer depends on a Recap. It
destroys this Node's `app.freeform` payloads by age or seq, as the operator
or the prune policy says. This is pre-release, so nothing is migrated.

## Considered options

- **Keep the Recap on the log, and read only one's own.** Rejected: every
  other Node stores and replicates a message it ignores, and reads another
  party's private framing of the talk, which the Transcript stays local to
  avoid ([ADR 0018](0018-an-agent-says-one-message.md)).
- **Keep every Recap a Node writes,** a ladder of files. Rejected for now:
  only the newest is read, and an earlier one can be kept by hand.
- **Keep Pruning bound to the Recap,** pruning only talk this Node's Recap
  covers. Rejected: the two answer different questions, what an agent
  reads and how much a replica keeps, and an operator may want either
  without the other. Pruning warns instead.
- **`RECAP.md` in the Conversation's Workspace.** Rejected: a Workspace holds
  settings only ([ADR 0015](0015-agent-profiles-handles-and-agent-chains.md)),
  and a Recap is data, like a Transcript. It is still plain markdown that an
  operator may read and edit.

## Consequences

- **A Conversation's data directory.** Each Conversation gets one directory
  under the Group's data directory, `group/<alias>/conversations/<conversation>/`,
  `<conversation>` being the hex Conversation ID or `group` for Group-level
  talk. It holds `RECAP.md` and `transcripts/<chain>.jsonl`, which move here
  from `group/<alias>/transcripts/<conversation>/` ([ADR 0018](0018-an-agent-says-one-message.md)).
- **`RECAP.md`** is markdown with a front matter giving `base_seq`, the
  highest seq it accounts for, which must name an `app.freeform` of that
  Conversation. `conversation recap` takes the text, as before, and writes
  the file whole through a temporary file and a rename, so a crash leaves
  the old Recap or the new one. Who writes the text is unchanged.
- **What agents read.** A summoned agent's talk is this Node's `RECAP.md`, as
  one `user` turn, then the talk after its `base_seq`. With no `RECAP.md` it
  is all the talk this replica holds. If Pruning has destroyed talk that no
  Recap covers, the preamble says earlier talk is missing.
- **`conversation log`** no longer folds: it shows the talk that has not been
  pruned, with pruned rows as placeholders under `--full`. `--lean` is
  `RECAP.md` and the talk after it, as an agent reads it.
- **`app.recap` (id 13) is retired** and stays reserved, along with the live
  Recap and the fold's rule that a Recap's `base_seq` must grow.
- **Pruning** is `conversation prune --before <seq>` or
  `--older-than <duration>`. Age is the envelope's `created_at`, as its sender
  claims it, so a sender can only change how long its own messages are kept.
  It warns when it would destroy talk past this Node's `RECAP.md`, since this
  Node's agents then lose that talk with no account in its place. The rest of
  ADR 0012's Pruning stands: payloads of `app.freeform` only, the one UPDATE
  the triggers admit, every row still verifying through `envelope_hash`.
- **The prune policy** becomes an age, `prune = "90d"` in a Group's
  `config.toml`; absent means an archival Node. `serve` applies it on start,
  after a snapshot restore, and once a day, in place of "when a Recap
  commits".
