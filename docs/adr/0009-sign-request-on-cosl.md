# Sign Requests are a distinct COSL type carrying structured transaction intent

A Sign Request is one Node's proposal that a Group's Key sign one or more
transactions. It is a new COSL payload type (`sign.request`), **not** an
overload of `session.start` or `app.freeform`. It sets `conversation_id` to the
target Key's identity — the first non-freeform message to do so, as
[ADR 0004](0004-conversation-on-cosl.md) anticipated — which names *which* of the
Group's Keys signs and pins the curve. Its body is a human-auditable transaction
intent (target, value, calldata, inputs/outputs, instructions), never opaque
bytes, so every Node independently reconstructs the canonical unsigned
transaction(s) and derives the identical set of **Digests** to sign. Design in
the [Signing Design doc](https://claude.ai/artifact/1hHd6qfr2nUpq1Evp3jnBz);
tickets to follow.

## Considered options

- **Sign opaque pre-hashed bytes supplied by the initiator** — rejected: Nodes
  would sign blind, the log would be unauditable, and the curve↔Network↔address
  validation that makes a wrong-chain or wrong-address spend fail up front would
  be impossible. The resulting signature is identical either way (EOA-
  indistinguishable); only the auditability differs, so structured intent wins
  at no on-chain cost.
- **Overload `session.start`** — rejected: a Sign Request has its own object,
  its own lifecycle, and its own reason-for-being. It initiates a signing
  Session (reusing the keygen machinery, [ADR 0010](0010-signing-session-semantics.md)),
  but conflating the proposal with the generic session-start envelope would hide
  the transaction intent inside a field that keygen never needed.
- **One Network per request** — rejected: a request is scoped to one Key, hence
  one curve, but the Key/address is valid on every Network of that curve. Each
  transaction therefore carries its **own** `network_config_id`, and networks of
  the same curve may be mixed in one request (e.g. several EVM chains for
  cross-chain arbitrage). Each individual signed transaction still carries its
  own chain id / network / blockhash in its Digest, so it is broadcastable on
  exactly one Network. Mixing Bitcoin and Ethereum in one request is permitted
  (both secp256k1); the rule is purely curve-based, with no special-casing.
- **One transaction per request** — rejected: a Bitcoin transaction needs one
  signature per input, and ordered Ethereum nonces (`n_0 … n_k`) express one
  semantic on-chain execution. A request therefore carries an ordered
  `transactions[]`; each contributes 1..N Digests, flattened across the array
  and all signed under one Session.

## Consequences

- New COSL registry type `sign.request` (id 7), with the `network.*` types from
  [ADR 0008](0008-networks-on-cosl.md) at ids 8–10.
- Body: `transactions[]` (each with its own `network_config_id` + per-network
  intent, discriminated by Network type), a **required** `reason` (free text,
  with an optional `{conversation_id, msg_id}` pointer to a Key-scoped
  Conversation or nil-conversation-id Group-level talk), and a `deadline`
  (UNIX seconds, default **300**; a tighter per-Network default may be needed for
  Solana, whose `recentBlockhash` expires in ~60–90s).
- Validation at creation: every referenced Network's required curve equals the
  Key's curve; all consensus-critical fields (nonce, gas/fees, UTXOs,
  blockhash) are present, because they are inside the Digest and `serve` performs
  no chain RPC to fill them — the chain-connected initiator supplies them.
- Per-network Digest rules: Ethereum RLP → keccak256 (EIP-1559); Bitcoin
  BIP-143/341 sighash per input; Solana compiled message bytes.

## As built (proposing and inspecting)

- `network_config_id` is the `msg_id` of the `network.create` / `network.update`
  row that defines the Network's config, so a request pins the exact config it
  was proposed against; inspect still resolves it after the alias is edited.
  A new request must reference a *live* config.
- `deadline` is stored as `deadline_seconds`, a window after the envelope's
  `created_at` (the proposer's default of 300 is written into the payload, so
  every Node reads the same value). Status was `open` or
  `expired` by that window alone (superseded by the #43 amendment in ADR 0010).
- `reason_ref` is an optional `{conversation_id, msg_id}` pointer.
- The proposer's envelope signature is its acceptance: one append, no separate
  accept message. This still stands: other Nodes accept with a `sign.accept`
  ([ADR 0010](0010-signing-session-semantics.md)), the proposer never does.
- Intents are `EthereumIntent` (EIP-1559; amounts as base-10 strings),
  `BitcoinIntent` (inputs with amount + scriptPubKey, outputs; fee implied) and
  `SolanaIntent` (fee payer, blockhash, instructions), enforced to match the
  Network's type. The writer rejects a request unless every field the Digest
  needs is present.
