# A signing Session refreshes its chain fields, broadcasts and confirms

Until now a Sign Request took three steps: propose, accept until t Nodes had
accepted (which starts and runs the signing Session), then a separate
`sign broadcast`. Broadcasting only reported the transaction id the chain's
node handed back, whether or not the transaction was ever mined. A Solana
request also had to be signed within about a minute of proposing, because
its `recentBlockhash` was read at propose and goes stale within 60–90
seconds. Now the t-th accept sets everything off. The Node that starts the
Session reads fresh chain fields into `session.start`. Once the Session
completes, that Node broadcasts and then watches each transaction until it is
mined, rejected or dropped, and records the result on the log as a **Transaction
Outcome**. The proposer and each other signer press one button each. This
amends [ADR 0009](0009-sign-request-on-cosl.md) and
[ADR 0010](0010-signing-session-semantics.md).

## Considered options

- **Refresh nothing; propose promptly.** Rejected: a request waiting on its
  t-th accept, or for the Session slot, is signed with a blockhash or nonce
  that may already be stale, and the chain rejects it after the Group has
  done all the work.
- **Each signer reads its own fresh fields.** Rejected: every signer must
  sign the same Digests, and two reads of the latest blockhash rarely agree.
  The fields are read once, by the Node that starts the Session, and put on
  the log.
- **Refresh fees too.** Rejected: a fee is a price the accepters agreed to.
  A blockhash only keeps the transaction alive, and a nonce only puts it in
  order. Neither changes what the transaction does.
- **Record outcomes only on the Node that broadcast.** Rejected: every Node
  needs to see that a transaction failed and can be retried, and an agent
  needs to see it too.

## Consequences

- **`session.start` carries a `sign_refresh`**: for some of the request's
  transactions, a new value for one field. That is `recent_blockhash` for
  Solana and `nonce` for Ethereum. Bitcoin has nothing to refresh. The Node
  appending the start reads them from its own Networks just before it does:
  the latest blockhash at `finalized`, and for each chain the Key's pending
  transaction count, rising in request order across that chain's
  transactions. A value it entered by hand is overwritten all the same. A
  chain it cannot read keeps its proposed values, and the Node logs why.
- **Every Node applies the first start's refresh** to the request before it
  derives the Digests, checks a signature or assembles a transaction
  (`signreq.Lookup`, `Signed`, `Inspect`). A refresh that names a transaction
  twice or out of range, or a field of another chain type, makes the request
  invalid for that start, so the start is aborted like any other invalid
  one. `sign inspect` shows the intent as signed, with the refreshed fields.
- **Solana's default deadline is no longer 60 seconds.** It existed only
  because the blockhash went stale. A Solana request now gets the normal
  default, since the blockhash is read at the start.
- **Automatic broadcast.** When a signing Session completes, the Node that
  appended its `session.start` broadcasts it, as `sign broadcast` does. If
  no receipt or Transaction Outcome appears within 30 seconds of the
  complete, any other signer does. Each Node broadcasts a request
  automatically at most once per run of serve, and only within 10 minutes
  of the complete, so a restart does not resubmit an old request. A
  rejected submission is recorded as an outcome rather than retried.
  `sign broadcast` stays as the manual retry.
- **`sign.outcome`** (registry id 17) is a Transaction Outcome:
  `{ sign_request_id, tx_index, tx_hash, result, block, detail }`, scoped to
  the Key's Conversation like the other sign messages. Its `result` is one
  of the following:
  - `confirmed`: mined. Solana at `confirmed` commitment. Ethereum when its
    receipt is `confirmations` blocks deep: a new per-Network setting,
    default 1. Bitcoin at one confirmation.
  - `reverted`: mined but failed. An Ethereum receipt with status 0, or a
    Solana transaction that landed with an error.
  - `rejected`: the chain's node refused the submission. `detail` says why.
  - `dropped`: the transaction can no longer be mined, or was not mined in
    time:
    - Solana: its blockhash is no longer valid and the transaction never
      landed.
    - Ethereum: its nonce was used by another transaction, or it was not
      mined within 10 minutes.
    - Bitcoin: it was not confirmed within 24 hours.

  `block` is the block number or slot of a mined transaction.
- **Who watches.** A Node watches each transaction it put in a receipt until
  that transaction has an outcome. A restarted serve picks up its receipts
  that have no outcome yet. Bitcoin is watched in the background like the
  others; its outcome simply comes later.
- **Statuses after `signed`**, read from each transaction's latest receipt
  or outcome in log order:
  - `broadcast`: every transaction went out.
  - `confirmed`: every transaction is confirmed.
  - `failed`: some transaction's latest word is a failure.

  A repeat broadcast after a failure moves the request back to `broadcast`.
  The desktop offers **Broadcast** only on a `failed` request whose failure
  a resubmission can fix (`rejected` or `dropped`). A reverted transaction
  is mined, so retrying it means a new Sign Request.
