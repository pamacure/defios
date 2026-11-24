# An agent describes a call and proposes a Draft

An Ethereum proposal took two hand copies of opaque bytes. The agent ran
`ethereum__tx__encode_calldata`, copied its base64 into the intent's `data`
for `chain_fill`, then copied `chain_fill`'s whole result, the calldata and
the filled nonce, gas and fees, into `sign_propose`. Models miscount long runs
of `A` (the base64 of zero bytes, which ABI padding is full of). In one run
an ERC-20 `transfer` lost one character. That time the length became invalid
and protojson refused it. Losing four characters would have decoded cleanly,
shifting every word after the slip into a different recipient or amount. The
form also took a `0x…` hex string as unpadded base64 without complaint, since
`0`, `x` and hex digits are all base64 characters.

The model should never carry bytes it cannot check. It describes the call,
and the Node encodes it. The Node keeps what it filled, and the agent proposes
that by name.

## Decisions

- **An Ethereum intent may give `call` in place of `data`.**
  ```json
  {"to": "0x…", "value": "0",
   "call": {"signature": "transfer(address to, uint256 amount)",
            "args": ["0x60d8…09e5", "100000"]}}
  ```
  The Node encodes it with `internal/evmabi`, as `encode_calldata` did, and
  puts the result in `data` before anything else reads the intent. Arguments
  follow `encode_calldata`'s rules: positional, every integer a string (a
  JSON number is refused, since it may already be rounded), an array or tuple
  a JSON array. Giving both `call` and `data` is refused.
- **`to` and `data` may be `0x` hex.** A bytes field whose string starts with
  `0x` is read as hex and refused if the hex is invalid. Anything else is
  base64, as before. `to` must be 20 bytes, and a mixed-case `0x` address
  must pass its EIP-55 checksum. This applies to every Ethereum bytes field
  in the input form.
- **One normaliser, in `signreq`.** It turns the input form into the intent
  protojson reads, and every path that takes transactions uses it:
  `sign_propose`, `chain_fill`, `sign propose` and `sign fill`. `chain_fill`
  also runs the checks `sign_propose` would (such as a 20-byte `to`) before
  reading the chain. A malformed intent is then reported as itself, not as
  the node's `eth_estimateGas` error.
- **The Sign Request is unchanged.** The proto and the COSL still hold bytes
  only. The signature text is not carried, so a reviewer decodes `data`
  independently and never relies on the proposer's description of it.
- **`chain_fill` returns a Draft.** Its result keeps the transactions and
  notes and adds `draft`, a short random id. The Node holds the filled
  transactions in memory under that id, scoped to the Conversation and its
  Key. A Draft lasts 10 minutes, and a serve restart loses it, in which case
  the agent fills again.
- **`sign_propose` takes a Draft.** It takes exactly one of `transactions`
  or `draft`, plus its `reason`. A Draft is proposed exactly as it was filled
  and cannot be edited: to change one, the agent fills again. A successful
  proposal uses it up. A refused one leaves it, so the agent can fix the
  reason and try again. `transactions` stays for intents `chain_fill` does
  not fill, such as Bitcoin's.
- **A note shows the call.** For an intent that gave `call`, `chain_fill`'s
  note decodes the encoded `data` back by that signature, as
  `transfer(address to = 0x60D8…09E5, uint256 amount = 100000)`. The note is
  read back from the bytes, so it shows what will be signed. The proposer
  checks it against the talk with no `decode_calldata` call.
- **`ethereum/tx` loses `encode_calldata`.** With `call` in place, all it
  produced was bytes to copy. `decode_calldata`, `address` and `units` stay,
  since reviewers need them.
- **The ethereum `propose` Skill** says: write `to` as `0x` hex and a contract
  call as `call`, fill, check the notes, then propose the Draft.

## Considered options

- **Only accept hex.** Hex lines up in 64-character words and a length check
  catches most slips, but the model would still copy the bytes twice.
  Rejected as the whole fix. Hex input is kept as a convenience, mainly for
  people pasting from an explorer.
- **`sign_propose` fills what is missing itself.** This removes the second
  copy, but the proposer would never see the nonce, fees and gas before
  proposing, which is the check [ADR 0029](0029-an-agent-profile-holds-the-whole-agent.md)
  asks for. A Draft removes the copy and keeps the check.
- **Carry `call` onto the Sign Request.** Reviewers would read the
  proposer's label for the bytes and not the bytes themselves. Rejected:
  reviewers decode on their own.
- **Keep `encode_calldata` for previews.** Rejected: a tool whose output is
  bytes invites copying them, and a note already shows the decoded call.
- **Drafts on disk.** Rejected: a Draft goes stale within minutes (fees, and
  a Solana blockhash), so a restart losing it costs only a refill.

## Consequences

- Amends ADR 0029 (`chain_fill` returns a Draft, and `sign_propose` takes
  one) and ADR 0042 (`ethereum/tx` loses `encode_calldata`; the input form
  widens with `call` and hex bytes, and the `tx` servers still read it).
- The CLI keeps `sign fill | sign propose` with full transactions: a pipe
  copies nothing by hand. It gets `call` and hex through the shared
  normaliser.
- Solana instructions also carry base64 `data` and have the same problem.
  They are left for a later ADR once this one has proved itself.
