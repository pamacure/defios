# Signing Sessions complete on a verifiable signature, not n-of-n agreement

A signing Session reuses the keygen machinery — `session.start` /
local-accept / `tss.session` rounds / `session.abort`, driven through the
`session.Engine` seam ([ADR 0003](0003-keygen-session-orchestration.md)) — but
its completion rule differs deliberately. A keygen Session is complete only when
every participant appends `session.complete` agreeing on the joint public key,
because a key share is not self-describing. A **signature is self-authenticating**:
it either verifies against the Key's public key or it does not. So a signing
Session completes the moment a valid signature exists for every Digest,
produced by the **t** participants who ran the protocol — not n-of-n. Design in
the [Signing Design doc](https://claude.ai/artifact/1hHd6qfr2nUpq1Evp3jnBz);
tickets to follow.

## Considered options

- **Reuse keygen's n-of-n `session.complete` rule verbatim** — rejected: it
  would make a t-of-n signature wait on the n−t Nodes that did not even
  participate, and they have nothing to attest — the signature already proves
  itself. The keygen rule exists because a minted share cannot be checked from
  the log; a signature can.
- **A separate local-accept for the proposer** — rejected: the proposer *signs*
  the `sign.request`, an active, intentional act that is proof in itself. The
  local-accept gate exists to stop an unattended `serve` from rubber-stamping a
  *peer's* start ([ADR 0003](0003-keygen-session-orchestration.md)); it does not
  apply to the Node that authored the request. `serve` records the proposer's
  accept atomically on propose, so proposing is a single step.
- **Only the originator may broadcast** — rejected: a signed transaction is
  self-authenticating and **idempotent** on-chain (identical bytes → identical
  txid; nonce / UTXO / blockhash ensure one inclusion), so letting any Node
  broadcast costs nothing in safety and buys liveness — it stops an offline or
  withholding originator from stranding a validly-signed transaction. Originator-
  first remains a convention, not a rule.

## Consequences

- No new COSL accept type for signing, consistent with
  [ADR 0003](0003-keygen-session-orchestration.md): membership gates on the
  `sign.request` commit; Vault/engine use gates on local accept. Once **t**
  participants (the proposer included) have accepted, the engine runs the
  tss-lib `ecdsa/signing` / `eddsa/signing` protocol once per Digest.
- `session.complete` carries the assembled signature set; a Node that cannot
  verify a produced signature against the Key aborts. Crash or the 2-minute
  `WaitingFor` timeout yields `session.abort`; there is no resume, and retry is a
  new Sign Request. A Sign Request past its `deadline` is `expired`.
- Broadcasting is an external action followed by a **required** `sign.broadcast`
  COSL receipt (a Broadcast Receipt: tx hash per transaction, broadcaster Node,
  timestamp, `network_config_id`), so broadcasting is provable and visible and
  double-broadcast is both harmless and detectable. An optional later
  `sign.confirmed` receipt may record on-chain confirmation.
- The signing `session.Engine` adapter lives alongside the `tssl` keygen engine
  behind the same seam, so a future Lockness `cggmp24` / `givre` signer swaps in
  the same way keygen does ([ADR 0003](0003-keygen-session-orchestration.md)).
