# Solana durable nonces

DeFiOS signs Solana transactions with a `recent_blockhash` only. A Sign
Request cannot use a durable nonce (a first `AdvanceNonceAccount` instruction
and the nonce account's stored nonce in place of the blockhash).

## Why this is out of scope

The point of a durable nonce would be to let a Solana Sign Request wait in
the Outbox ([ADR 0025](../docs/adr/0025-an-outbox-holds-what-a-node-writes-without-a-quorum.md))
for longer than a blockhash lives, about 60 to 90 seconds. To get that, the
project would have to take on:

- **A nonce account lifecycle.** Something has to create, fund and initialise
  the nonce account. A Key is the only signer a Solana transaction may have
  (the fee payer), so the Key would have to be the nonce authority, and the
  account would have to be created with `CreateAccountWithSeed` with the Key
  as base. A fresh keypair account would need a signature of its own.
- **Reading the chain before accepting.** Before accepting, each Node would
  have to read the nonce account from its own Solana endpoint
  ([ADR 0022](../docs/adr/0022-members-are-trusted-for-their-own-messages.md))
  and check that its stored nonce matches. Today no Node reads a chain before
  it accepts. The only chain call is at broadcast.
- **Signatures that outlive their request.** A signed durable-nonce
  transaction stays valid on-chain until the nonce is advanced, long after the
  request's `deadline`. A Sign Request's deadline would then limit only when a
  signature can be produced, not when it can be broadcast.

Instead, a Solana Sign Request goes into the Outbox like any other queueable
message. If a majority comes back before its deadline, the request commits
and can be accepted and broadcast. If not, the Outbox drops it as expired
unsent. This is the simpler trade: the Outbox makes no promise about
expiry on the chain's side.

## Prior requests

- #88: "Solana Sign Requests with a durable nonce"
