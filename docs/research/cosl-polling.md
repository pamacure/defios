# What serve's COSL polling costs: benchmark record

Measured October 2026 at `1c253c6`, on an Intel i5-10210U (4 cores, 8
threads), Linux, Go 1.27. The decision this supports is
[ADR 0014](../adr/0014-serve-reads-the-cosl-on-change.md). This file is the
before/after record: each change made under that ADR adds its run beside
[`before.txt`](cosl-polling/before.txt), named for its ticket
(`after-54.txt`), and a subsection under **After**.

## The question

A live `serve` can sit idle for days. Three loops read the COSL on a timer.
What does one wake cost, and what does that add up to while nothing is
happening?

| Loop | Tick | What each wake reads |
| --- | --- | --- |
| Session layer, `session.(*Service).poll` | 50 ms | every row with its envelope; decodes every row; re-checks the signatures of every completed signing Session |
| Conversation Workspaces, `serve.(*server).ensureWorkspaces` | 1 s | every row with its envelope; decodes every row to list Keys |
| Prune policy, `recap.(*Keeper).Step` (#52) | 1 s | the latest `app.recap` row |

## Method

`internal/cosl/coslbench` builds a two-Node Group's replica through the FSM,
so every row is real: signed, chained and inserted exactly as `serve`
inserts it. Round traffic dominates the bytes, so its size comes from a
measured run of the tss-lib engines in `internal/engine/tssl` with two
parties:

| Session | `tss.session` rows | Bytes |
| --- | --- | --- |
| ECDSA keygen | 8 | 449,191 |
| ECDSA signing, one Digest | 20 | 21,494 |
| EdDSA keygen | 6 | 1,088 |

Talk is about 60 tokens a message, half Group-level and half in the signing
Key's Conversation, with an `app.recap` every 1,000 messages. Every signing
Session completes with a signature that really verifies, so the Session
layer does the crypto it does in production.

| Shape | Talk | ECDSA Keys | EdDSA Keys | Signings | Rows | Envelope bytes |
| --- | --- | --- | --- | --- | --- | --- |
| small | 1,000 | 2 | 1 | 10 | 1,294 | 1.6 MB |
| medium | 10,000 | 5 | 2 | 100 | 12,685 | 9.8 MB |
| large | 50,000 | 10 | 5 | 500 | 63,207 | 42.3 MB |

Each benchmark measures one wake against that replica:

- `*IdleWake`: nothing has changed since the last wake, which is every wake
  of an idle serve.
- `SessionWakeAfterARow`: one message of talk has committed since the last
  wake, which is the most a wake has to react to.

Run it again with:

```bash
go test ./internal/session/ ./internal/serve/ ./internal/recap/ -run '^$' \
  -bench 'IdleWake|WakeAfterARow' -benchtime 10x -count 6 > after.txt
go run golang.org/x/perf/cmd/benchstat@v0.0.0-20261009192801-be2c69fb417e \
  docs/research/cosl-polling/before.txt after.txt
```

The replicas are cached under `$TMPDIR/defios-coslbench/`; the first run
builds them, which takes about a minute.

## Results

### Baseline (`before.txt`, at `1c253c6`)

Per wake, median of 6:

| Benchmark | small | medium | large |
| --- | --- | --- | --- |
| SessionIdleWake | 10.0 ms, 8.3 MiB | 92.6 ms, 62.8 MiB | 451 ms, 294 MiB |
| SessionWakeAfterARow | 11.4 ms | 100 ms | 479 ms |
| WorkspaceIdleWake | 9.3 ms, 8.3 MiB | 79.3 ms, 62.6 MiB | 378 ms, 293 MiB |
| KeeperIdleWake | 0.11 ms, 3.7 KiB | 0.12 ms | 0.12 ms |

What that comes to on an idle serve, a projection from those numbers rather
than a measurement. A `time.Ticker` drops ticks while its reader is busy, so
a wake longer than its tick runs back to back:

| Loop | small | medium | large |
| --- | --- | --- | --- |
| Session layer | 20% of a core | a whole core | a whole core |
| Workspaces | 0.9% | 7.9% | 38% |
| Prune policy | 0.01% | 0.01% | 0.01% |

On the large replica the Session layer also allocates about 650 MiB a
second for the garbage collector. Idle cost is not its only cost: a Session
reacts to a round only on a wake, so on the large replica every one of a
signing Session's 20 rounds waits up to about half a second, which adds
seconds to every signature.

### Where a Session wake goes

CPU profile of `SessionIdleWake/large`:

| | Share |
| --- | --- |
| `cosl.(*Store).Log`: reading every row out of sqlite, `scanRow` | 48% |
| `parse`: protobuf-decoding every envelope and payload | 17% |
| `VerifyECDSA`: re-checking 1,000 completes' signatures | 17% |
| Garbage collection | about 10% |

None of that work has a result that changed since the last wake.

### After

#### #53: signature checks remembered per row ([`after-53.txt`](cosl-polling/after-53.txt))

The Session layer remembers whether each `session.complete` row's
signatures verify, keyed by the row's envelope hash, so a wake re-checks
none of the completes it has already seen. Per wake, median of 6, against
the baseline:

| Benchmark | small | medium | large |
| --- | --- | --- | --- |
| SessionIdleWake | 7.65 ms (−24%) | 69.6 ms (−25%) | 334 ms (−26%) |
| SessionWakeAfterARow | 7.58 ms (−34%) | 82.6 ms (~) | 334 ms (~) |

Allocation barely moves (−0.2% bytes, −0.7% allocs), since checking a
signature allocates little next to reading and decoding every row. The run
was noisy: another build shared the machine, the WakeAfterARow spreads
reach ±123%, and `WorkspaceIdleWake`, whose code did not change, also ran
9–17% faster. So read the saving as roughly the 17% the profile gave
`VerifyECDSA`, not as the full quarter. An idle wake on the large replica
still reads and decodes every row, which the remaining ADR 0014 tickets
deal with.

#### #54: the replica announces change; the Workspace watcher wakes on it (`after-54.txt`)

[`after-54.txt`](cosl-polling/after-54.txt), same machine and method.
`cosl.Store` now announces each committed row and each restore
(`Store.Changed`), and the Workspace watcher waits on that or on the
one-minute fallback tick (`cosl.Follower`). A wake first reads the
replica's `Mark` (`last_applied_seq` from `cosl_meta`, and the restore
count) and stops there if neither moved since the last wake it finished.
If one did, it looks for a `session.complete` after the seq it last acted
on, and lists Keys from `session.start`/`complete`/`abort` rows only
(`key.Read`) if it finds one or the replica was restored.

`WorkspaceIdleWake` measures that gated wake, which is what the fallback
tick costs an idle serve; between ticks the watcher does no work at all.
Per wake, median of 6:

| Benchmark | small | medium | large |
| --- | --- | --- | --- |
| WorkspaceIdleWake, before | 9.3 ms, 8.3 MiB | 79.3 ms, 62.6 MiB | 378 ms, 293 MiB |
| WorkspaceIdleWake, after | 20.8 µs, 624 B | 22.2 µs, 624 B | 25.6 µs, 624 B |

The cost no longer grows with the replica: 22 allocations a wake on every
shape. On an idle serve the watcher goes from 38% of a core on the large
replica to one 26 µs read a minute.

This run was made on a branch without #53, so its Session numbers are the
baseline's within noise (`SessionIdleWake/large` 421 ms) and say nothing
about #53. `KeeperIdleWake` is unchanged at 0.11 ms.

#### #56: the prune policy wakes on change and rereads its config (`after-56.txt`)

[`after-56.txt`](cosl-polling/after-56.txt), same machine and method. The
prune policy now waits on `Store.Changed` or the one-minute fallback tick,
as the Workspace watcher does. A wake reads the replica's `Mark` and stops
there if it has not moved; if it has, `recap.Keeper`'s own gate reads the
latest `app.recap` row and prunes only after a new Recap or a restore. With
the policy off, a wake touches nothing, and only the fallback tick wakes
it. On that tick it also stats the Group's `config.toml` and rereads it if
its modification time changed, so `prune` takes effect without a restart.

`PrunePolicyIdleWake` is new: the gated wake with the policy on, which is
what an idle serve's prune policy costs at each fallback tick, less the
stat. `KeeperIdleWake` is re-run as the issue asked; it measures the
Keeper's gate, which a wake now reaches only when the Mark moved. Per wake,
median of 6:

| Benchmark | small | medium | large |
| --- | --- | --- | --- |
| KeeperIdleWake (#54 run) | 106 µs, 3.7 KiB | 111 µs | 110 µs |
| KeeperIdleWake (this run) | 131 µs, 3.7 KiB | 133 µs | 127 µs |
| PrunePolicyIdleWake | 28.5 µs, 624 B | 32.1 µs, 624 B | 31.3 µs, 624 B |

Between ticks an idle serve's prune policy does no work at all, where #52's
ran the Keeper's 0.11 ms gate every second. It now costs one ~30 µs read a
minute, 22 allocations, on every shape.

This run is noisy. Other agents were building and testing on the machine
(load average 11 as it started), and code this ticket did not touch ran
slower too: `WorkspaceIdleWake` +13–27%, `KeeperIdleWake` +15–24% with
spreads up to ±66%, and `SessionIdleWake/large` 562 ms ± 56% against #53's
334 ms. Read the absolute times as upper bounds. The allocation counts,
which load does not move, are the same as #54's.

Building this turned up a lost-row hazard. The FSM's append began a
deferred sqlite transaction, read `cosl_meta`, then wrote. A `Prune`
committing in between made that write fail at once with `SQLITE_BUSY`,
without waiting out `busy_timeout`, and the entry was applied with no row.
Waking on change made it likely, because the policy now prunes just as a
Recap commits, while the next rows are still being applied. The Store now
begins every transaction `IMMEDIATE` (`_txlock=immediate`), so a write
waits for the lock instead.

#### #57: listing Keys reads Session rows only ([`after-57.txt`](cosl-polling/after-57.txt))

Against [`before-57.txt`](cosl-polling/before-57.txt), same machine and
method, run with:

```bash
go test ./internal/cli/ -run '^$' -bench KeyListing -benchtime 10x -count 6
```

Every reader that lists Keys and nothing else (`key list` and
`key inspect`, `--key` and Conversation resolution in the CLI, the
Conversation check on `conversation send`, `sign propose`, a broadcast's
Workspace overrides, the prune policy's Conversation names) now reads
`session.start`, `session.complete` and `session.abort` rows only
(`key.Read`). A Sign Request lookup, which the Session layer makes for
every signing Session and a broadcast makes once, reads only the request
types, the Networks and those Session rows (`signreq.LookupMsgTypes`).
Readers that already hold the whole log for another fold (`sign list`,
`sign inspect`) still list Keys from it, but `key.List` now decodes only
the Session rows among them.

`KeyListing` is one `--key` resolution: list every Key and resolve a
public-key prefix. Per listing, median of 6:

| Benchmark | small | medium | large |
| --- | --- | --- | --- |
| KeyListing, before | 7.54 ms, 8.3 MiB | 67.7 ms, 62.5 MiB | 330 ms, 293 MiB |
| KeyListing, after | 316 µs, 175 KiB | 2.39 ms, 1.4 MiB | 12.9 ms, 6.8 MiB |

About 96% less time and 98% less memory on every shape. What remains grows
with the number of Sessions, not with talk: the large replica holds 515
Sessions, and each signing Session's start and completes are still read to
learn that it minted no Key. The machine was shared with two other builds
during both runs (load average 2.5 to 4.5 on 8 threads), so the medium
baseline spreads ±64%; the large and small runs are within ±15%.
