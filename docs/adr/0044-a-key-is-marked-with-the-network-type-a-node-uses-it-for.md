# A Key is marked with the Network type a Node uses it for

The desktop's Conversations rack shows an icon on each row, so that the
Group's talk and each Key's Conversation can be told apart at a glance. A
Key's curve does not say which icon to show: an ECDSA Key signs for Bitcoin
and for Ethereum and other EVM chains alike, and an EdDSA Key for Solana now
and for other chains later. So a Node records, per Key, the **Network type**
it uses that Key for, in the top-level `network_type` field of that Key's
Conversation Workspace `config.toml`, beside its alias
([ADR 0033](0033-a-key-alias-starts-on-the-log-and-then-is-the-nodes.md)).
With none, an EdDSA Key is taken to be for Solana and an ECDSA Key for
nothing in particular. As with the alias, whoever starts the keygen Session
may propose it on `session.start` (`proposed_network_type`), so every Node
party to the Key begins with the same one; from then on it is each Node's
own, and the log is not read for it again. The desktop proposes it from the
New Key form, beneath the curve, and changes it from the Key's About panel;
the CLI proposes it with `session start --network-type`.

## Considered options

- **Inferring it from the Key's Sign Requests or this Node's Networks** —
  rejected: a Key with no Sign Request yet would have no icon, and a Node
  with both Bitcoin and Ethereum Networks could not tell. Being explicit
  costs one choice per Key.
- **A Group-wide setting kept in sync on the log** — rejected for the same
  reason as a Group-wide alias: it is a label, and every other label belongs
  to the Node that uses it. Only the starting value is on the log.
- **Setting it on each Node separately, with no proposal** — rejected for
  the reason ADR 0033 gives for aliases: usually only one Node would set it.
- **An override at `session accept`, as the alias has** — not now: a Node
  that wants another type sets it once the Key exists, which costs nothing,
  whereas an alias override exists because a taken alias must be resolved
  before the mint.
- **Restricting what the Key may sign to its Network type** — rejected:
  a Sign Request already names the chain of each transaction, and the
  setting exists to mark a row, not to guard a Key. A Key may in principle
  sign for every chain of its curve, and still can.

## Consequences

- `network_type` is one of `bitcoin`, `ethereum` or `solana`. The Group's
  `config.toml` refuses it, and it is not inherited through the merge.
- A proposal that is not a Network type of the Key's curve is refused by
  `session start` and, arriving from another Node, ignored: it cannot block
  a keygen. A good one is written only into a Workspace the Node has just
  created, so a restart does not bring a changed type back.
- Setting it checks that the type fits the Key's curve. A hand-edited type
  that does not fit is ignored, and the Key falls back to its curve's
  default.
- The desktop's About panel shows the Key's addresses for its Network type:
  the Ethereum address for Ethereum, the Solana address for Solana, and for
  Bitcoin the P2WPKH address on each bitcoin network of the Conversation's
  Networks (mainnet if it has none). An ECDSA Key with no type set shows
  both its Ethereum and its Bitcoin address, and never just one of them.
- It needs no serve, as an alias does not.
- No CLI verb changes it after keygen yet. Only the desktop shows it, and the
  file can be edited by hand.
