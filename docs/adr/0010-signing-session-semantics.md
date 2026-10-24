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

- ~~No new COSL accept type for signing, consistent with
  [ADR 0003](0003-keygen-session-orchestration.md).~~ **Superseded** by the
  `sign.accept` type below. Membership gates on the `sign.request` commit;
  Vault/engine use gates on the Node's own accept. Once **t**
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

_Signers are chosen by first-t accepts, as described in the amendment below._

- A signing Session is a `session.start` with `sign_request_id` set (the
  `sign.request`'s msg_id). Its `participants` are the **t** signers (the first t
  Nodes to accept, sorted), not the whole Roster; `t` is the Key's t and `n` the
  Roster size. Nonce boxes are sealed to the signers only. A start whose signers
  are not distinct Roster Nodes, are not the first t accepts, or mismatch the Key
  is invalid and aborted by every Node. Choosing a different signer set means a
  new Sign Request.
- `sign propose` appends only the request (the proposer's accept is its signature).
  `sign accept <msg-id>` appends a `sign.accept` and opens the local gate;
  `sign decline <msg-id>` aborts the Session once one exists. A signing Session
  shares the one-open-Session slot with keygen.
- The engine (`tssl.NewSigner`) runs `ecdsa/signing` once per Digest in parallel
  inside one engine, prefixing each message with the Digest's index so the COSL
  carries a single `tss.session` stream. Each Digest's party gets its own session
  nonce, derived from the sealed one. EdDSA signing is wired too, see "As built (signing and broadcasting Solana)" below.
- `session.complete` carries `signatures`: one 65-byte `r||s||v` per Digest
  (low-s, `v` the recovery id 0/1, as an Ethereum transaction wants it). A
  complete counts only if every signature verifies against the Key's public key
  and the Digests derived from the request, and its sender is a signer; the first
  such complete finishes the Session. A Node whose own signatures do not verify
  aborts instead of appending. A signing Session never mints or deletes a share.
- A Session still open past the request's deadline is aborted
  (`sign request expired`). Ethereum and Bitcoin transactions start a Session on
  an ECDSA Key and Solana transactions on an EdDSA Key (Solana and Bitcoin: see
  below); a request with a transaction of no known type stays proposed without one.

## Amendment: signers are the first t to accept (`sign.accept`)

The as-built rule above had the proposer pick the signers (itself plus the first
t−1 other Roster Nodes, sorted) and name them in `session.start`. That made the
Roster's order, not willingness, decide who signs, and an offline named Node timed
the Session out while other Nodes were available. It also cannot express a
signer set for t ≥ 3 that is not yet known when the request is proposed.

- **A new COSL type, `sign.accept`** (registry id 11), body `{ sign_request_id }`,
  scoped to the Key's Conversation like `sign.request`; its sender is the
  accepting Node. [ADR 0003](0003-keygen-session-orchestration.md) rejected an
  explicit COSL accept for **keygen** because (a) joining the Group was already
  gated by Group formation, (b) the participant set is the whole Roster, so no
  selection is needed, and (c) local presence is a separate job from membership.
  Signing differs on (b): signers are chosen from a larger set, so the log must
  record who agreed. (a) and (c) still hold, and the local-accept gate is kept —
  `sign accept` is still the deliberate operator step; only now it is an append.
  This supersedes "no new COSL accept type for signing".
- **Signer set** = the first t distinct Nodes to have accepted, in COSL (Raft)
  log order, the proposer first (its signature on the `sign.request` is its accept,
  [ADR 0009](0009-sign-request-on-cosl.md), so `sign propose` appends only the
  request). Later accepts are ignored. A duplicate accept, or one made after the
  request's deadline, does not count; the FSM already drops non-Roster senders.
- **`session.start`** is appended once t signers exist, by any signer that has
  accepted locally; the first in log order counts, and a later start for the same
  request is invalid and aborted by every Node (alongside the one-open-Session slot).
  Whoever appends it mints the nonce and seals it to the t signers. A start is valid
  only if its `participants` equal the first t accepts before it, in sorted order,
  and its sender is one of them.
- **Decline** before a start exists is a no-op: not accepting is declining, and an
  append already made cannot be withdrawn. `sign decline` says which happened (the
  Session was aborted; there was nothing to decline; your accept stands; the Session
  had already finished). After a start exists it aborts the Session.
- A request whose Key is not on the log shows `key_known: false` and `?` for t in
  `sign list` / `inspect`, not `0`.
- An aborted Session (timeout, decline, crash) still kills the request: retry is a
  new Sign Request, and accepts do not carry over. A request that never reaches t
  accepts before its deadline is `expired`.

## As built (broadcasting Ethereum)

- **`sign.broadcast`** (registry id 12), body `{ sign_request_id, transactions: [{ network_config_id, tx_hash, tx_index }] }`,
  scoped to the Key's Conversation. Its sender is the broadcasting Node and its
  envelope `created_at` is the broadcast time, so the receipt adds no fields for them.
  A receipt covers one Network's transactions: a non-empty subset of the request's,
  each entry naming its `tx_index` in `transactions[]` (two transactions on one Network
  are otherwise indistinguishable) and all on one `network_config_id`. Readers drop a
  receipt that is empty, repeats or overruns an index, names a Network other than the
  request's for that index, or mixes Networks.
- `sign broadcast <msg-id>` (any Node, any number of times) takes the signatures of the
  request's completed Session (`session.Signatures`: the first complete that verifies),
  assembles each transaction (`signreq.AssembleEthereum`: the unsigned EIP-1559 field list
  plus `y_parity, r, s`) and submits it to its Network with `eth_sendRawTransaction`,
  then commits one receipt per Network. The endpoints are this Node's local override for the alias if
  it has one, else the Network's current `rpc_urls`; they are tried in order and only
  http(s) ones can carry the call. A request with no verified signature is `not-signed`.
- **Idempotent.** The same signatures give the same bytes and hash. A node that rejects the
  submission but already has that hash (`eth_getTransactionByHash`), e.g. "already known" or
  "nonce too low" after inclusion, counts as success and is reported as `already_known`. A
  repeat broadcast therefore succeeds and commits one more receipt; receipts are never
  deduplicated, so the repeat stays visible.
- **All-or-error per Network.** Transactions are grouped by `network_config_id` and each
  group is submitted independently, in request order. A group that fully goes out commits
  its own receipt; a group where any submission is rejected (a rejection the node does not
  recognise) commits none, reports its error, and does not stop the other groups. Earlier
  transactions of a failed group may already be on the network, which is why a re-run is
  safe: receipted groups report `already_known` and add a repeat receipt, the failed group
  is retried. `sign broadcast` prints per-Network success or failure and exits non-zero
  if any failed; `--json` carries the same.
- `sign list` shows a receipt count and whether the request is fully broadcast (every
  transaction appears in some receipt); `sign inspect` shows each receipt.

## As built (multi-transaction requests)

- A request's `transactions[]` needs no special path: Digests are flattened in
  transaction order under one Session, each transaction is bound to its own
  Network's chain id, and `sign broadcast` sends each to its own Network's rpc
  urls and records all of them in one `sign.broadcast` receipt. Tests cover
  ordered nonces on one Network mixed with a second Network of the same curve.

## As built (signing and broadcasting Solana)

- **Digest** of a Solana transaction is its legacy compiled message
  (`signreq.SolanaMessage`), not a hash: Ed25519 hashes internally, so the Node signs
  the message bytes. Digests are therefore byte slices (`session.Params.Digests`),
  a 32-byte hash for ECDSA and the message for EdDSA. Accounts are ordered
  signer-writable, signer-readonly, writable, readonly, each by first appearance,
  and a repeated account merges its flags. Only the fee payer may be a signer: the
  Key signs for one address, so any other signer is refused at propose, as is a
  fee payer that is not the Key's address and a transaction over 1232 bytes.
- The engine (`tssl.NewSigner`) runs `eddsa/signing` per message the same way as
  ECDSA, with the message length passed to tss-lib so a leading zero byte stays in
  the hash. `session.complete` carries one 64-byte `R||S` per message; a complete
  counts only if each verifies as RFC 8032 against the Key (`session.VerifyEd25519`).
- `sign broadcast` assembles `shortvec(1) || signature || message` and submits it
  with `sendTransaction` (base64). A transaction's id is its signature, so the
  receipt's `tx_hash` is 64 bytes for Solana (32 for Ethereum) and is shown base58.
  Idempotent like Ethereum: a rejected send whose signature `getSignatureStatuses`
  already finds without an error is `already_known`.
- **Default deadline**: a request with a Solana transaction and no explicit deadline
  gets **60** seconds rather than 300, since its `recentBlockhash` lives ~60–90s.
  The window is written into the payload as before.

## As built (signing and broadcasting Bitcoin)

- **A Key maps to one script type: P2WPKH** (witness v0, `0014<hash160(compressed
  pubkey)>`), `signreq.P2WPKHScript`. Every input of a Bitcoin transaction must
  spend that script, so a request that spends an output the Key does not own, or any
  other script type, is refused at propose and again by every Node deriving Digests.
  **P2TR is deferred, not rejected on principle**: BIP-341 needs a BIP-340 Schnorr
  signature, which the tss-lib ECDSA engine cannot produce; it needs a Schnorr
  threshold signer (FROST/Givre) behind the same `session.Engine` seam. Outputs may
  pay any address type (P2PKH, P2SH, P2WPKH, P2WSH, P2TR), decoded for the Network's
  bitcoin network (`signreq.OutputScript`; `mainnet` is `bc`/`0x00`/`0x05`, `testnet`
  and `signet` `tb`/`0x6f`/`0xc4`, `regtest` `bcrt`), so a wrong-network address
  fails up front like a wrong-curve Network does.
- **Digests**: one per input, the BIP-143 SIGHASH_ALL sighash (`signreq.BitcoinDigests`),
  a 32-byte double-SHA-256 like Ethereum's, so the ECDSA signer and `VerifyECDSA` are
  unchanged. A request's Digests are flattened across its transactions, so a mixed
  Ethereum + Bitcoin request (both secp256k1) signs one Digest for the former and one
  per input for the latter under one Session, and `signreq.Signed` takes the
  signatures the same way.
- The intent carries no `sequence`: every input is `0xfffffffd` (BIP-125 opt-in RBF,
  and below `0xffffffff` so a non-zero `lock_time` is enforced), the version is 2, and
  the fee is the inputs minus the outputs. A different sequence is a new intent field.
- `sign broadcast` assembles the segwit transaction (`signreq.AssembleBitcoin`): each
  input's witness is `DER(r,s) || 0x01` and the Key's compressed pubkey (the Session's
  signatures are already low-s). A transaction's id is its txid, the receipt's `tx_hash`
  is 32 bytes and shown hex in display order, as explorers write it. It submits with
  Bitcoin Core's `sendrawtransaction` over the Network's http(s) endpoints
  (`btcrpc.Send`), idempotent like the others: a node that rejects the submission but
  already has the txid (`getrawtransaction`), or says it is already in the block chain
  (a mined one on a node without a txindex), is `already_known`.
- Verified against independent implementations: the sighashes, signed bytes and txid
  equal btcd's `txscript` for the same intent, and the serve test's fake node accepts a
  transaction only if btcd's script engine executes every input's witness against the
  output it spends. Not covered: fee policy and a real `bitcoind` (regtest).

## Amendment: request lifecycle decisions (#43)

- **Request status** is derived from the log, not the deadline alone: `open`
  (collecting accepts), `waiting` (t Nodes accepted, no Session yet, for example
  while another Session holds the one-open-Session slot), `signing` (Session
  open), `signed` (Session finished), `aborted` (Session aborted) and `expired`
  (deadline passed with no Session). A Session outlives the deadline, so `signed`
  and `aborted` beat `expired`. Whether a signed request has been broadcast stays
  in `sign list`'s `broadcasts=` and `fully` columns, not a state of its own.
- **Late accept** is a strong error, `ErrSignerSetFull` (wire kind
  `signer-set-full`, HTTP 409), whether or not the Session has started yet, and
  appends nothing. `ErrNotParticipant` is now only for keygen's `session accept`.
  The decision uses the Node's local, possibly stale, view of the log; a UI may
  choose to ignore the error.
- **Backdated accepts**: the deadline check keeps using each row's own
  `created_at`. Every Node runs the same code, which commits its timestamp
  honestly; a Node that backdates is outside the trust model, as with the
  proposer's own `created_at`, which anchors the deadline.
- **Busy slot**: unchanged. `startAccepted` waits while another Session is open
  and retries on the next poll; the request shows `waiting` meanwhile, and
  expires if its deadline passes first. The other Session is bounded by the
  2-minute `WaitingFor` timeout, so only short deadlines (Solana's 60s default)
  can be hit.
