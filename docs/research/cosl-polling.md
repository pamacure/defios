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

The other loops are not changed by #54 and measure as before, within noise:
`SessionIdleWake/large` 421 ms, `SessionWakeAfterARow/large` 421 ms,
`KeeperIdleWake` 0.11 ms.
