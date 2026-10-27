# serve reads the COSL when it changes, not on a clock

`serve`'s long-running readers of the COSL (the Session layer, the
Conversation Workspace watcher and the prune policy) wake when the replica
changes, when something in the process asks them to, or when a deadline of
their own falls due. None of them re-reads the replica on a timer to find
out whether anything happened. Each reads only the message types it folds,
and a fact that cannot change once committed is worked out once.

Measured first ([cosl-polling](../research/cosl-polling.md)): on a replica
of 63k rows and 42 MB, which is ten ECDSA Keys, five hundred signatures and
50k messages of talk, one wake of the Session layer reads and decodes every
row and re-checks every signature ever produced. That takes 451 ms against
a 50 ms tick, so an idle `serve` keeps a core busy indefinitely. The
Workspace watcher adds 38% of another core for the same reason. Nothing
either of them computes on an idle wake differs from the wake before.

## Considered options

- **A slower tick** (say 1 s for the Session layer). Rejected: each wake
  still costs time linear in the whole history, so a large replica still
  costs 45% of a core while idle. It also makes keygen and signing slower,
  because a Session reacts to a round only when it wakes.
- **A cheap gate on the existing tick**: compare `last_applied_seq` and the
  restore count, and do the full wake only if either moved. This is what
  `recap.Keeper` does, at 0.12 ms a wake. Rejected as the main mechanism:
  idle cost becomes small but never zero (72,000 sqlite reads an hour from
  the Session layer alone), and it cuts no latency. Kept as the fallback
  below.
- **Watching the sqlite file** (inotify or similar on the WAL). Rejected:
  every append to the replica happens inside this process, so the process
  already knows exactly when one happened, without platform-specific file
  events or a write that is not yet committed.
- **sqlite's update hook**. Rejected for the same reason. It also fires per
  row inside the transaction, before commit, so it would need deferring to
  the commit anyway, and it ties the design to one driver.
- **Holding the decoded replica in memory**. Rejected: memory equal to the
  replica, 300 MiB of objects at 42 MB, for every serve, to answer questions
  that need a few hundred of its rows.
- **Incremental folds now**: each reader keeps its folded state and applies
  only rows after the last seq it saw, rebuilding after a restore. Deferred,
  not rejected. It is the largest change, and the cost it removes (a full
  read per committed row while a Session runs) is only worth paying down
  once the cheaper changes have been measured.

## Consequences

- **`cosl.Store` announces change.** After every append the FSM commits and
  after every restore that rewrites the replica, the Store wakes whoever is
  waiting on it (a channel closed and replaced, so any number of readers
  wait at no cost). This covers every write that matters: the FSM is the
  replica's only appender and runs inside `serve`, and `Restore` runs
  inside `serve`. The one write from outside the process,
  `conversation prune` from the CLI, only destroys payloads, which no reader
  needs to react to.
- **The Session layer waits** for a change, a nudge, or a timer, whichever
  comes first:
  - **Nudges.** Every in-process event it acts on nudges it: a local accept
    or decline, a keygen start, and an engine's output or result.
  - **Its own deadline.** The timer is set for its nearest deadline: the
    accept wait (`WaitingFor`) of a Session waiting on this Node, an
    engine's stall timeout, or a Sign Request's deadline. With no Session
    open and no Sign Request pending, no timer is set. The 50 ms tick goes,
    and a round is acted on as soon as it commits.
- **The Workspace watcher and the prune policy wait for a change.** The
  Workspace watcher creates a Workspace only for a Key minted since its last
  wake. The prune policy keeps `recap.Keeper`'s gate, so a change that is not
  a Recap or a restore costs it one row.
- **A one-minute fallback tick** remains on each loop, in case a wake is
  ever missed. It compares `last_applied_seq` and the restore count with
  what the loop last acted on and stops there if neither moved, so an idle
  minute costs one `cosl_meta` read per loop. The prune policy also uses
  that tick to reread `config.toml` when its modification time changes, so
  turning `prune` on or off needs no restart, as
  [ADR 0013](0013-group-config-workspaces-and-named-secrets.md) arranges
  for every other local setting. This amends #52, where `serve` read the
  policy once at start.
- **Reads are narrowed to what a reader folds.** `cosl.Query` gains a set of
  message types and an after-seq bound. Listing Keys reads `session.start`,
  `session.complete` and `session.abort` rows only, so talk and round
  traffic are never decoded to find a Key, whether by the Workspace watcher,
  a broadcast or a CLI verb.
- **Settled facts are worked out once.** Whether a `session.complete` row's
  signatures verify cannot change once the row is committed, so the Session
  layer remembers the answer by row and never checks those signatures
  again.
- **Errors are logged when they change.** A loop that fails the same way on
  every wake logs once, again if the error changes, and once when it clears.
- **The benchmark is the record.** `internal/cosl/coslbench` and the
  `*IdleWake` and `SessionWakeAfterARow` benchmarks stay in the tree. Each
  change made under this ADR adds its run to
  [cosl-polling](../research/cosl-polling.md). The target for every loop's
  idle wake is under 1 ms on the large replica, and zero wakes while idle
  apart from the fallback tick.
- **Incremental folds** wait until these changes are measured. If a Session
  wake after one committed row still costs more than a round's worth of
  latency on the large replica, that is the next ticket.
  Measured after #53–#57: a wake after one round's row still reads every
  Session row ever committed, 113 ms on the large replica, so its cost grows
  with the Group's signing history. That is #58. The state the Session layer
  holds between wakes is a small summary per Session, not the Session's
  rows, so it never becomes the in-memory replica rejected above.
