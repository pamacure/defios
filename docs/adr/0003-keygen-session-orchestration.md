# Keygen Sessions commit on the log; engine Start waits on local accept

A keygen Session is one COSL-coordinated engine run that may mint one Key. Group
membership is the approval gate on the log: `session.start` commit is implicit
join — there is no COSL accept type. Engine `Start()`, unsealing `tss.session`,
and signing round envelopes wait on a *local* accept so an unlocked `serve`
cannot rubberstamp a peer's start. tss-lib (and Lockness) cannot resume a
crashed run, so failure is `session.abort` and retry is a new Session ID. Decided
in [defios#6](https://github.com/pamacure/defios/issues/6); local accept amended
in [defios#8](https://github.com/pamacure/defios/issues/8).

## Considered options

- **Explicit COSL accept** (each participant appends a matching `session.start`) —
  rejected: [Design Group formation and membership mechanics](https://github.com/pamacure/defios/issues/5)
  already gated joining the Group. A second wire round would surprise a 2-Node
  Raft commit that cannot land unless both Nodes are already applying. Local
  presence is a different job, handled below.
- **Engine `Start()` on apply alone** — rejected in
  [Design Node storage: encrypted keyshare container and COSL sqlite schema](https://github.com/pamacure/defios/issues/8):
  an unattended `serve` would participate in any `session.start` that commits,
  so a live 2-of-2 Group mints Keys whenever one operator is at a keyboard.
  Replica apply stays automatic (public roster keys only); Vault use waits on
  local accept. The initiator's `session start` CLI *is* that Node's accept.
  No new COSL type.
- **Resume a crashed Session from COSL history** — rejected: neither tss-lib nor
  Lockness resumes in-memory party state. Replay of `tss.session` rows after a
  restart would feed a new party a transcript it did not produce. Abort, then a
  new operator-initiated `session.start`.
- **Parallel Sessions** — rejected for MVP as a simplicity rule, not a safety
  property. The envelope already demuxes by Session ID; crash/abort/UX are
  simpler with one in-flight mint. Many Keys may exist on one Group; they are
  minted in series.
- **t always = n (n-of-n only)** — rejected: t is chosen per Session
  (DeFiOS min-signers) with `n = |Roster|`. Shareholders are never a subset of
  the Roster. The engine adapter maps onto tss-lib as `threshold = t − 1`.
- **Any t in `2 ≤ t ≤ n`** — rejected in
  [Design the Key-scoped group chat channel](https://github.com/pamacure/defios/issues/7):
  2-of-4 (and any `2t ≤ n`) admits disjoint signing sets, so two cabals can
  produce valid signatures on conflicting spends. `session.start` accepts t
  only when `2t > n` and `t ≤ n` (2-of-2, 2-of-3, 3-of-4, 3-of-5, …). This is
  Key policy, not a COSL commit rule: Raft majority of the Roster still
  commits every row, including `app.freeform`
  ([ADR 0004](0004-conversation-on-cosl.md)).
- **Session ID as Key identity** — rejected: a Session is a run; a Key is the
  joint public key it produced. Identity is curve + those public key bytes.
- **`SetSessionNonce` = Session ID** — rejected: the preimage must stay secret
  and the Session ID is public on the log. The initiator mints 32 random bytes
  and seals a copy to each participant's Encryption keypair inside
  `session.start`.
- **PartyID.Key = auth public key** — rejected in
  [ADR 0002](0002-group-formation.md): Node ID is the Shamir x-coordinate and
  the `tss.session` routing tag (nil = broadcast).

## Consequences

- `session.start` payload is Session ID, curve, t, n, participants (= Roster),
  and the sealed nonce boxes. t that fails `2t > n` (or t > n), or a start while
  another Session is open, is rejected at the Session layer; a racing start is
  aborted by Session ID. This amends
  [Define the COSL message envelope and type registry](https://github.com/pamacure/defios/issues/4):
  start carries the sealed nonce; the routing tag is Node ID, not the auth
  public key.
- Every Node applies `session.start` to the replica on commit. Engine `Start()`
  (and subsequent unseal/sign for that Session) runs only after local accept —
  the initiator accepted by submitting. Round messages are `tss.session`. A Key
  exists when every participant has appended `session.complete` agreeing on the
  same joint public key; mismatch discards the local share and aborts. Silence
  until the 2-minute `WaitingFor` still yields `session.abort`.
  ([ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md)).
- Live `WaitingFor` beyond 2 minutes, or a crash that loses in-memory party
  state, yields `session.abort` once the COSL is writable. Aborted rows stay on
  the log. n>2 (later): keygen still needs all n live; an offline participant
  times out and aborts.
- ECDSA Paillier preparams exist before `Start()`, generated at init into the
  Vault. A share is written to the Vault when `endCh` fires, before this Node
  appends `session.complete`; abort or mismatch deletes it.
  ([ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md)).
- Conversations key off the Key (curve + joint public key), not the Session
  ([Design the Key-scoped group chat channel](https://github.com/pamacure/defios/issues/7);
  [ADR 0004](0004-conversation-on-cosl.md)).
- MVP acceptance is two sequential 2-of-2 Sessions (ECDSA and Ed25519, t=2,
  n=2) on a 2-Node Group.

