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

## As built (signing one Ethereum transaction)

- A signing Session is a `session.start` with `sign_request_id` set (the
  `sign.request`'s msg_id). Its `participants` are the **t** signers — the
  proposer plus the first t−1 other Roster Nodes, sorted — not the whole Roster;
  `t` is the Key's t and `n` the Roster size. Nonce boxes are sealed to the
  signers only. A start whose signers are not distinct Roster Nodes, omit the
  proposer, or mismatch the Key is invalid and aborted by every Node. Choosing a
  different signer set means a new Sign Request.
- `sign propose` appends the request and then the start, which is the proposer's
  accept. `sign accept <msg-id>` is another signer's local accept (nothing is
  written to the COSL); `sign decline <msg-id>` aborts the Session. A signing
  Session shares the one-open-Session slot with keygen.
- The engine (`tssl.NewSigner`) runs `ecdsa/signing` once per Digest in parallel
  inside one engine, prefixing each message with the Digest's index so the COSL
  carries a single `tss.session` stream. Each Digest's party gets its own session
  nonce, derived from the sealed one. EdDSA signing is not wired yet.
- `session.complete` carries `signatures`: one 65-byte `r||s||v` per Digest
  (low-s, `v` the recovery id 0/1, as an Ethereum transaction wants it). A
  complete counts only if every signature verifies against the Key's public key
  and the Digests derived from the request, and its sender is a signer; the first
  such complete finishes the Session. A Node whose own signatures do not verify
  aborts instead of appending. A signing Session never mints or deletes a share.
- A Session still open past the request's deadline is aborted
  (`sign request expired`). Only Ethereum transactions on an ECDSA Key start a
  Session; any other request stays proposed without one.
